package hooks

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// A ws-hook whose pane disappears while the daemon cannot be reached exits,
// even when it knows no agent pid (#553). Before, the gone-pane probe ran only
// for a known agent pid that had died, so this hook retried forever.
func TestWSHookExitsWhenItsPaneGoesWhileTheDaemonIsUnreachable(t *testing.T) {
	// A private tmux server, always named by its socket: never the live one.
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	srv.Start("-s", "gone", "sleep 60")
	tm := srv.Run
	pane, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "gone:", "sh")
	if err != nil {
		t.Fatal(err, pane)
	}
	// The hook's bare tmux calls reach the private server through TMUX.
	t.Setenv("TMUX", srv.TMUX())
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

// Only tmux reached and the pane absent is "gone". A tmux that cannot be
// reached (no server, a socket path too long) exits 1 too, and must not make
// a hook retire a live peer.
func TestPaneGoneNeedsTmuxToAnswer(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	srv.Start("-s", "probe", "sleep 60")
	dir := srv.Dir
	live, err := srv.Run("display-message", "-p", "-t", "probe:", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", srv.TMUX())
	if paneGone(live) {
		t.Fatal("a live pane reads as gone")
	}
	if !paneGone("%9999") {
		t.Fatal("a pane the server does not have reads as present")
	}
	for name, socket := range map[string]string{
		"no server":            filepath.Join(dir, "none"),
		"socket path too long": "/tmp/" + strings.Repeat("x", 120) + "/sock",
	} {
		t.Setenv("TMUX", socket+",1,0")
		if paneGone(live) || paneGone("%9999") {
			t.Fatalf("%s: an unreachable tmux read as an answer", name)
		}
	}
}

// One gone answer is not enough to end a hook, and any other answer resets
// the count: a terminal offline retires the peer.
func TestGoneNeedsTwoAnswersInARow(t *testing.T) {
	n, done := nextGoneStrikes(0, true)
	if done {
		t.Fatal("one gone answer ended the hook")
	}
	if n, done = nextGoneStrikes(n, false); done || n != 0 {
		t.Fatalf("a present answer did not reset the count: %d %v", n, done)
	}
	n, _ = nextGoneStrikes(n, true)
	if _, done = nextGoneStrikes(n, true); !done {
		t.Fatal("two gone answers in a row did not end the hook")
	}
}
