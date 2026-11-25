package tmuxtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// Neither Run nor the shim lets a caller name another server.
func TestCallerCannotNameAnotherServer(t *testing.T) {
	srv := New(t)
	srv.Start("-s", "probe", "sleep 30")
	if _, err := srv.Run("-S", "/tmp/elsewhere", "list-sessions"); err == nil || !strings.Contains(err.Error(), "refusing global option") {
		t.Fatalf("Run with -S: %v", err)
	}
	for _, opt := range []string{"-S", "-L", "-f"} {
		out, err := exec.Command("tmux", opt, "elsewhere", "list-sessions").CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 97 || !strings.Contains(string(out), "refusing global option") {
			t.Fatalf("shim with %s: %v %s", opt, err, out)
		}
	}
}

// Isolate's environment reaches no server, and keeps refusing the sockets
// that were live before it.
func TestIsolatedEnvironment(t *testing.T) {
	if os.Getenv("TMUX") != "" || !strings.Contains(os.Getenv("TMUX_TMPDIR"), "rwtmuxbase") {
		t.Fatalf("TestMain did not isolate: TMUX=%q TMUX_TMPDIR=%q", os.Getenv("TMUX"), os.Getenv("TMUX_TMPDIR"))
	}
	for p := range outside {
		if !LiveSockets()[p] {
			t.Fatalf("an outside socket %s is no longer refused", p)
		}
	}
}

// Under FollowEnv a bare new-session starts a server at the sandbox's own
// default socket; cleanup stops that one too.
func TestFollowEnvStrayServerIsStopped(t *testing.T) {
	var strayPID int
	t.Run("sandbox", func(t *testing.T) {
		srv := New(t, FollowEnv())
		srv.Start("-s", "probe", "sleep 30")
		if out, err := exec.Command("tmux", "new-session", "-d", "-s", "stray", "sleep 30").CombinedOutput(); err != nil {
			t.Fatalf("bare new-session: %v %s", err, out)
		}
		stray := filepath.Join(srv.Dir, "tmux-"+strconv.Itoa(os.Getuid()), "default")
		out, err := exec.Command("tmux", "-S", stray, "display-message", "-p", "#{pid}").Output()
		if err != nil {
			t.Fatalf("no stray server at %s: %v", stray, err)
		}
		if strayPID, err = strconv.Atoi(strings.TrimSpace(string(out))); err != nil || strayPID <= 0 {
			t.Fatalf("stray server pid %q: %v", out, err)
		}
	})
	if strayPID == 0 {
		return
	}
	// The socket is gone with the sandbox dir; the process must be too.
	for i := 0; i < 50; i++ {
		if syscall.Kill(strayPID, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the stray sandbox server (pid %d) outlived the test", strayPID)
}

func TestNoPTYReadsOnlyTmuxOutput(t *testing.T) {
	if NoPTY(&Error{Args: []string{"new-session", "echo fork failed"}, Output: "duplicate session: x", Err: errString("exit status 1")}) {
		t.Fatal("NoPTY matched an argument")
	}
	if !NoPTY(&Error{Args: []string{"respawn-pane"}, Output: "respawn pane failed: fork failed: Device not configured", Err: errString("exit status 1")}) {
		t.Fatal("NoPTY missed tmux's diagnostic")
	}
}
