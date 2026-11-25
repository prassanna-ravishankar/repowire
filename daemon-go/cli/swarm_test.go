package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `swarm snapshot -o FILE` writes the manifest owner-only and never replaces
// a file without --force; with the experiment off it fails with the daemon's
// own explanation.
func TestSwarmSnapshotWritesAPrivateFile(t *testing.T) {
	enabled := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swarm/snapshot" {
			http.NotFound(w, r)
			return
		}
		if !enabled {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"swarm snapshot is an experiment: set experiments.swarm_snapshot: true and restart the daemon"}`))
			return
		}
		_, _ = w.Write([]byte(`{"schema":1,"taken_at":"2026-11-25T23:00:00Z","machine":"m","panes":[{"session":"work","pane_id":"%1"}]}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}
	out := filepath.Join(t.TempDir(), "swarm.json")

	if code := swarmCommand(c, []string{"snapshot", "-o", out}); code != 0 {
		t.Fatalf("snapshot exit %d", code)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file %v mode %v", err, info.Mode().Perm())
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), `"pane_id": "%1"`) {
		t.Fatalf("manifest = %s", raw)
	}
	if code := swarmCommand(c, []string{"snapshot", "-o", out}); code == 0 {
		t.Fatal("replaced an existing snapshot without --force")
	}
	if code := swarmCommand(c, []string{"snapshot", "-o", out, "--force"}); code != 0 {
		t.Fatalf("--force exit %d", code)
	}
	if code := swarmCommand(c, []string{"snapshot", "-o"}); code != 2 {
		t.Fatalf("-o without a file: exit %d", code)
	}
	enabled = false
	if code := swarmCommand(c, []string{"snapshot"}); code == 0 {
		t.Fatal("snapshot succeeded with the experiment off")
	}
}
