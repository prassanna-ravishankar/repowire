package hub

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

func TestAttachmentsExpireWithContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("old.png", 8*24*time.Hour)
	write("new.png", 6*24*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, now := context.Background(), time.Now()
	expire := func(p state.RetentionPolicy) int {
		n, err := expireAttachments(ctx, dir, p.ContentCutoff(now), nil)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := expire(state.RetentionPolicy{}); n != 0 {
		t.Fatal("the keep-forever policy removed a file")
	}
	if n := expire(state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 180 * 24 * time.Hour}); n != 1 {
		t.Fatalf("removed %d files, want 1", n)
	}
	for name, want := range map[string]bool{"old.png": false, "new.png": true, "sub": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}

	// A file an open ask may still name lives as long as the ask can.
	write("asked.png", 2*time.Hour)
	write("stale.png", 25*time.Hour)
	expire(state.RetentionPolicy{Content: time.Hour, AskLife: 24 * time.Hour})
	for name, want := range map[string]bool{"asked.png": true, "stale.png": false} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}

// A directory full of expired files does not refuse an upload: the upload
// expires them itself instead of waiting for the hourly sweep.
func TestUploadExpiresStaleFilesBeforeRefusingAtTheCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := AttachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.bin")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(stale, maxAttachmentDirSize); err != nil { // sparse: no real disk
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	upload := func(h *Hub) int {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", "note.txt")
		_, _ = part.Write([]byte("hello"))
		_ = form.Close()
		req := httptest.NewRequest(http.MethodPost, "/attachments", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		h.uploadAttachment(rec, req)
		return rec.Code
	}

	if code := upload(&Hub{}); code != http.StatusInsufficientStorage {
		t.Fatalf("keep-forever policy, full dir: status %d, want 507", code)
	}
	h := (&Hub{}).WithRetention(state.RetentionPolicy{Content: 7 * 24 * time.Hour}, nil)
	if code := upload(h); code != http.StatusOK {
		t.Fatalf("full of expired files: status %d, want 200", code)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the expired file is still there")
	}
}

type failingReferrer struct{}

func (failingReferrer) AttachmentReferences(context.Context) ([]state.AttachmentRef, error) {
	return nil, errors.New("database is locked")
}

// A file's life runs from its latest reference, not its upload (#573): one
// uploaded a week ago and attached to an ask today lasts as long as that
// ask's message, then goes with it. A file nothing names still goes a
// content lifetime after its upload.
func TestAnAttachmentLivesAsLongAsTheMessagesNamingIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, now := context.Background(), time.Now()
	week := 7 * 24 * time.Hour
	policy := state.RetentionPolicy{Content: week, Records: 180 * 24 * time.Hour}
	dir := AttachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uploaded := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-week - time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	byID, byPath, unnamed := uploaded("a1b2c3d4.png"), uploaded("e5f6a7b8.pdf"), uploaded("0c0c0c0c.txt")
	exists := func(when string, want map[string]bool) {
		t.Helper()
		for path, keep := range want {
			if _, err := os.Stat(path); (err == nil) != keep {
				t.Errorf("%s: %s exists=%v, want %v", when, filepath.Base(path), err == nil, keep)
			}
		}
	}

	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, att := range []map[string]any{{"id": "a1b2c3d4"}, {"path": byPath}} {
		if err := store.InsertPeerMessage(ctx, &state.PeerMessage{Kind: state.MessageAsk, PeerID: "repow-b",
			Text: "look", Attachments: []map[string]any{att}, CreatedAt: now.UTC().Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	h := (&Hub{}).WithRetention(policy, store)

	// A lookup that fails removes nothing.
	if n, err := expireAttachments(ctx, dir, policy.ContentCutoff(now), failingReferrer{}); n != 0 || err == nil {
		t.Fatalf("a failed lookup: removed %d, err %v", n, err)
	}
	exists("after a failed lookup", map[string]bool{byID: true, byPath: true, unnamed: true})

	if got := h.ExpireAttachments(ctx, policy, now)["attachments.delete"]; got != 1 {
		t.Fatalf("removed %d files, want 1", got)
	}
	exists("while the asks hold them", map[string]bool{byID: true, byPath: true, unnamed: false})

	// A week on, the sweep has dropped the messages and the files go too.
	later := now.Add(week + time.Hour)
	if _, err := store.Sweep(ctx, policy, later); err != nil {
		t.Fatal(err)
	}
	if got := h.ExpireAttachments(ctx, policy, later)["attachments.delete"]; got != 2 {
		t.Fatalf("after the messages went: removed %d files, want 2", got)
	}
	exists("after the messages went", map[string]bool{byID: false, byPath: false})
}
