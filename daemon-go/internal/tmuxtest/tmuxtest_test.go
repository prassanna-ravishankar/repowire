package tmuxtest

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// A bare `tmux`, as the code under test runs it, reaches the sandbox even
// when $TMUX names another server.
func TestBareTmuxReachesOnlyTheSandbox(t *testing.T) {
	var srv *Server
	var dir, socket string
	t.Run("sandbox", func(t *testing.T) {
		srv = New(t)
		dir, socket = srv.Dir, srv.Socket
		if _, err := srv.Run("list-sessions"); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("a command before the server started ran: %v", err)
		}
		srv.Start("-s", "probe", "sleep 30")
		t.Setenv("TMUX", "/nonexistent/elsewhere,1,0")
		out, err := exec.Command("tmux", "display-message", "-p", "-t", "probe:", "#{socket_path}").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != srv.Socket {
			t.Fatalf("bare tmux reached %q (%v), want %s", out, err, srv.Socket)
		}
	})
	// The subtest's cleanup stopped its server and removed the sandbox.
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("sandbox socket outlived the test: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("sandbox dir outlived the test: %v", err)
	}
}

// With FollowEnv the environment, not a shim, picks the server, and it
// points only at the sandbox: TMUX cleared, TMUX_TMPDIR the sandbox.
func TestFollowEnvPointsTheEnvironmentAtTheSandbox(t *testing.T) {
	srv := New(t, FollowEnv())
	if os.Getenv("TMUX") != "" || os.Getenv("TMUX_TMPDIR") != srv.Dir {
		t.Fatalf("TMUX=%q TMUX_TMPDIR=%q, want empty and %s", os.Getenv("TMUX"), os.Getenv("TMUX_TMPDIR"), srv.Dir)
	}
	srv.Start("-s", "probe", "sleep 30")
	t.Setenv("TMUX", srv.TMUX())
	out, err := exec.Command("tmux", "display-message", "-p", "-t", "probe:", "#{socket_path}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != srv.Socket {
		t.Fatalf("bare tmux under FollowEnv reached %q (%v), want %s", out, err, srv.Socket)
	}
}

func TestLiveSocketsNameWhatABareClientWouldReach(t *testing.T) {
	t.Setenv("TMUX", "/some/live/sock,42,0")
	t.Setenv("TMUX_TMPDIR", "/some/tmpdir")
	live := LiveSockets()
	for _, want := range []string{"/some/live/sock", "/some/tmpdir/tmux-" + strconv.Itoa(os.Getuid()) + "/default", "/tmp/tmux-" + strconv.Itoa(os.Getuid()) + "/default"} {
		if !live[want] {
			t.Errorf("LiveSockets() = %v, missing %s", live, want)
		}
	}
}

func TestNoPTY(t *testing.T) {
	for msg, want := range map[string]bool{
		"respawn pane failed: fork failed: Device not configured": true,
		"create window failed: fork failed: Resource temporarily": true,
		"can't find session: mesh":                                false,
	} {
		if got := NoPTY(errString(msg)); got != want {
			t.Errorf("NoPTY(%q) = %v", msg, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
