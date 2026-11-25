package hub

// routes_attachments.go owns the "attachments" HTTP route group: filesystem-
// backed file uploads referenced from mesh messages. Port of
// repowire/daemon/routes/attachments.py.
//
//	POST /attachments        multipart upload → {id, path, filename, size, content_type}
//	GET  /attachments/{id}   stream a previously-uploaded file by id-prefix (404 if absent)
//
// Files live under ~/.repowire/attachments. Limits (matching Python): 10MB per
// file (413), 200MB total directory cap (507), and expiry with other content
// under the retention policy (ExpireAttachments, a pass of the store's
// sweep): a file lives while a stored message still names it, and a
// content lifetime past its upload otherwise.

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/state"
)

const (
	maxAttachmentFileSize = 10 * 1024 * 1024  // 10MB
	maxAttachmentDirSize  = 200 * 1024 * 1024 // 200MB
)

// AttachmentsDir returns ~/.repowire/attachments (created on demand by callers).
func AttachmentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".repowire", "attachments")
	}
	return filepath.Join(home, ".repowire", "attachments")
}

func (h *Hub) registerAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /attachments", h.requireAuth(h.uploadAttachment))
	mux.HandleFunc("GET /attachments/{attachment_id}", h.requireAuth(h.getAttachment))
}

// attachmentDirSize totals the bytes used by the attachments dir. Best-effort.
func attachmentDirSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func (h *Hub) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	// Cap the multipart parse so a huge upload can't exhaust memory; the file
	// itself is streamed to disk below with an exact byte-count guard.
	if err := r.ParseMultipartForm(8192); err != nil && err != http.ErrNotMultipart {
		writeError(w, http.StatusUnprocessableEntity, "malformed multipart form: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "missing 'file' field")
		return
	}
	defer file.Close()

	// Early reject on a declared size over the per-file cap (Python checks
	// file.size first). header.Size is the multipart-declared length.
	if header.Size > maxAttachmentFileSize {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("File too large (max %dMB)", maxAttachmentFileSize/1024/1024))
		return
	}

	dir := AttachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create attachments dir: "+err.Error())
		return
	}

	// The store's sweep expires files hourly; an upload that would hit the
	// cap clears expired files itself rather than wait for it.
	used := attachmentDirSize(dir)
	if used+max(header.Size, 0) > maxAttachmentDirSize {
		if n, err := expireAttachments(r.Context(), dir, h.attachmentRetention.ContentCutoff(time.Now()), h.attachmentRefs); err != nil {
			log.Printf("attachments: kept every file at the cap: %v", err)
		} else if n > 0 {
			used = attachmentDirSize(dir)
		}
	}
	if used >= maxAttachmentDirSize {
		writeError(w, http.StatusInsufficientStorage,
			fmt.Sprintf("Attachments directory full (%dMB used, %dMB cap). Files expire after retention.content_days.",
				used/1024/1024, maxAttachmentDirSize/1024/1024))
		return
	}

	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".bin"
	}
	id := uuid.NewString()[:8]
	dest := filepath.Join(dir, id+ext)

	out, err := os.Create(dest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open attachment for write: "+err.Error())
		return
	}

	var size int64
	buf := make([]byte, 8192)
	for {
		n, rerr := file.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > maxAttachmentFileSize {
				out.Close()
				_ = os.Remove(dest)
				writeError(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("File too large (max %dMB)", maxAttachmentFileSize/1024/1024))
				return
			}
			if used+size > maxAttachmentDirSize {
				out.Close()
				_ = os.Remove(dest)
				writeError(w, http.StatusInsufficientStorage,
					fmt.Sprintf("Attachments directory would exceed cap (%dMB). Files expire after retention.content_days.",
						maxAttachmentDirSize/1024/1024))
				return
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				_ = os.Remove(dest)
				writeError(w, http.StatusInternalServerError, "failed writing attachment: "+werr.Error())
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			_ = os.Remove(dest)
			writeError(w, http.StatusInternalServerError, "failed reading upload: "+rerr.Error())
			return
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		writeError(w, http.StatusInternalServerError, "failed closing attachment: "+err.Error())
		return
	}

	filename := header.Filename
	if filename == "" {
		filename = filepath.Base(dest)
	}
	contentType := header.Header.Get("Content-Type")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"path":         dest,
		"filename":     filename,
		"size":         size,
		"content_type": contentType,
	})
}

func (h *Hub) getAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attachment_id")
	if id == "" {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	dir := AttachmentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	for _, e := range entries {
		name := e.Name()
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if stem == id {
			full := filepath.Join(dir, name)
			w.Header().Set("Content-Disposition", "attachment; filename="+name)
			http.ServeFile(w, r, full)
			return
		}
	}
	writeError(w, http.StatusNotFound, "Attachment not found")
}

// attachmentReferrer lists the attachments stored messages still carry
// (state.Store.AttachmentReferences).
type attachmentReferrer interface {
	AttachmentReferences(ctx context.Context) ([]state.AttachmentRef, error)
}

// WithRetention gives the attachment routes the retention policy, so an
// upload that would hit the directory cap can expire files first, and
// where the files' references live. A nil refs means no message holds one.
func (h *Hub) WithRetention(p state.RetentionPolicy, refs attachmentReferrer) *Hub {
	h.attachmentRetention, h.attachmentRefs = p, refs
	return h
}

// ExpireAttachments is the attachment files' retention pass, plugged into
// the store's sweep (state.RetentionPlugs). It runs after the tables, so the
// references it reads are the content the sweep kept. Best-effort.
func (h *Hub) ExpireAttachments(ctx context.Context, p state.RetentionPolicy, now time.Time) map[string]int {
	n, err := expireAttachments(ctx, AttachmentsDir(), p.ContentCutoff(now), h.attachmentRefs)
	if err != nil {
		log.Printf("retention: kept every attachment: %v", err)
	}
	return map[string]int{"attachments.delete": n}
}

// expireAttachments removes regular files in dir modified before cutoff
// that no stored message names. A file lives as long as the content that
// references it, so one uploaded long ago and attached to a new ask lasts
// as long as that ask's message (#573); a file nothing names goes a content
// lifetime after its upload. A zero cutoff keeps everything, and so does a
// failed lookup of the references.
func expireAttachments(ctx context.Context, dir string, cutoff time.Time, refs attachmentReferrer) (int, error) {
	if cutoff.IsZero() {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	named := map[string]bool{}
	if refs != nil {
		list, err := refs.AttachmentReferences(ctx)
		if err != nil {
			return 0, err
		}
		for _, r := range list {
			// An upload's id is its file's stem; its path ends in the file.
			if r.ID != "" {
				named[r.ID] = true
			}
			if r.Path != "" {
				named[filepath.Base(r.Path)] = true
			}
		}
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if named[name] || named[strings.TrimSuffix(name, filepath.Ext(name))] {
			continue
		}
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) && os.Remove(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n, nil
}
