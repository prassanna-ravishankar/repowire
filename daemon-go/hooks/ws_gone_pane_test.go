package hooks

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A ws-hook whose pane disappears while the daemon cannot be reached exits,
// even when it knows no agent pid (#553). Before, the gone-pane probe ran only
// for a known agent pid that had died, so this hook retried forever.
func TestWSHookExitsWhenItsPaneGoesWhileTheDaemonIsUnreachable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "sock")
	// A private tmux server, always named by its socket: never the live one.
	tm := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-S", sock}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := tm("new-session", "-d", "-s", "gone", "sleep 60"); err != nil {
		t.Skipf("cannot start a tmux server: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = tm("kill-server") })
	pane, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "gone:", "sh")
	if err != nil {
		t.Fatal(err, pane)
	}
	server, err := tm("display-message", "-p", "-t", "gone:", "#{socket_path},#{pid},0")
	if err != nil {
		t.Fatal(err)
	}
	// The hook's bare tmux calls reach the private server through TMUX.
	t.Setenv("TMUX", server)
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX_PANE", pane)
	t.Setenv("REPOWIRE_AGENT_PID", "")
	t.Setenv("REPOWIRE_PEER_ID", "")
	t.Setenv("REPOWIRE_HOOK_LOCK_FD", "")

	// The daemon is unreachable: the config names a port nothing listens on,
	// so the hook can never reach a real daemon.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".repowire"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "daemon:\n    host: 127.0.0.1\n    port: " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".repowire", "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() { done <- RunWS() }()
	select {
	case code := <-done:
		t.Fatalf("the hook exited (%d) while its pane was alive", code)
	case <-time.After(1500 * time.Millisecond):
	}
	if out, err := tm("kill-pane", "-t", pane); err != nil {
		t.Fatal(err, out)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the hook kept retrying after its pane was gone")
	}
}
