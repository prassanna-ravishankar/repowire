package hub

import (
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

	now := time.Now()
	if n := expireAttachments(dir, state.RetentionPolicy{}.ContentCutoff(now)); n != 0 {
		t.Fatal("the keep-forever policy removed a file")
	}
	if n := expireAttachments(dir, state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 180 * 24 * time.Hour}.ContentCutoff(now)); n != 1 {
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
	expireAttachments(dir, state.RetentionPolicy{Content: time.Hour, AskLife: 24 * time.Hour}.ContentCutoff(now))
	for name, want := range map[string]bool{"asked.png": true, "stale.png": false} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}
