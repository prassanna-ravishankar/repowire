package hub

// routes_attachments.go owns the "attachments" HTTP route group: filesystem-
// backed file uploads referenced from mesh messages. Port of
// repowire/daemon/routes/attachments.py.
//
//	POST /attachments        multipart upload → {id, path, filename, size, content_type}
//	GET  /attachments/{id}   stream a previously-uploaded file by id-prefix (404 if absent)
//
// Files live under ~/.repowire/attachments. Limits (matching Python): 10MB per
// file (413), 200MB total directory cap (507), and a 24h TTL swept on every
// upload so a stale-but-expired dir doesn't deny a legitimate upload.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maxAttachmentFileSize = 10 * 1024 * 1024  // 10MB
	maxAttachmentDirSize  = 200 * 1024 * 1024 // 200MB
	attachmentMaxAgeHours = 24
)

// attachmentsDir returns ~/.repowire/attachments (created on demand by callers).
func attachmentsDir() string {
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

// cleanupExpiredAttachments removes files older than the TTL. Best-effort.
func cleanupExpiredAttachments(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-attachmentMaxAgeHours * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
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

	dir := attachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create attachments dir: "+err.Error())
		return
	}

	// Sweep TTL'd files first so a stale-but-expired dir doesn't deny a legit upload.
	cleanupExpiredAttachments(dir)

	used := attachmentDirSize(dir)
	if used >= maxAttachmentDirSize {
		writeError(w, http.StatusInsufficientStorage,
			fmt.Sprintf("Attachments directory full (%dMB used, %dMB cap). Wait for the %dh TTL to clear older files.",
				used/1024/1024, maxAttachmentDirSize/1024/1024, attachmentMaxAgeHours))
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
					fmt.Sprintf("Attachments directory would exceed cap (%dMB). Wait for the %dh TTL to clear older files.",
						maxAttachmentDirSize/1024/1024, attachmentMaxAgeHours))
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
	dir := attachmentsDir()
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
