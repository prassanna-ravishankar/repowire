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

	(&Hub{}).expireAttachments(dir) // no policy: keep everything
	if _, err := os.Stat(filepath.Join(dir, "old.png")); err != nil {
		t.Fatal("the zero policy removed a file")
	}

	h := (&Hub{}).WithRetention(state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 180 * 24 * time.Hour})
	h.expireAttachments(dir)
	for name, want := range map[string]bool{"old.png": false, "new.png": true, "sub": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}
