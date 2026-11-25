// Package tmuxtest gives a test its own tmux server, reachable only through
// an explicit socket path, so a test can never touch the live server that
// runs the mesh's agents.
//
// A tmux client without -S or -L picks its server from the environment: the
// socket named in $TMUX, else "default" under $TMUX_TMPDIR (or /tmp when that
// directory is missing). Tests run inside tmux panes, so a bare `tmux
// kill-server` in a test's cleanup reaches the live server. Here every call
// names the sandbox socket with -S, the code under test reaches tmux through a
// PATH shim that does the same, the sandbox refuses a socket that is the live
// server's, and kill-server runs only while the sandbox socket exists.
package tmuxtest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Server is one test's private tmux server.
type Server struct {
	// Socket is the server's socket path; every call names it with -S.
	Socket string
	// Dir is the sandbox directory holding the socket and the PATH shim.
	Dir string
	bin string
	t   testing.TB
}

// Option adjusts New.
type Option func(*options)

type options struct{ followEnv bool }

// FollowEnv leaves bare `tmux` calls to the environment instead of the PATH
// shim, for code whose subject is how tmux reads $TMUX (hooks run inside a
// pane). The environment still points only at the sandbox: TMUX is cleared,
// and TMUX_TMPDIR is the sandbox directory, which outlives the server. A test
// that sets TMUX itself must point it at the sandbox (Server.TMUX) or at a
// path that does not exist.
func FollowEnv() Option { return func(o *options) { o.followEnv = true } }

// New prepares a private server for t: a short sandbox directory (macOS caps
// unix socket paths at 104 bytes), a tmux shim first on PATH that pins every
// bare `tmux` call to the sandbox socket with an empty config, TMUX and
// TMUX_PANE cleared and TMUX_TMPDIR set to the sandbox, so nothing falls back
// to the live server. The server itself starts with the first Run of
// new-session. Skips when tmux is not installed.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "rwtmux")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Socket: filepath.Join(dir, "sock"), Dir: dir, bin: bin, t: t}
	if live := LiveSockets(); live[s.Socket] {
		_ = os.RemoveAll(dir)
		t.Fatalf("tmuxtest: refusing socket %s: it is the live server's", s.Socket)
	}
	// Registered first, so it runs last: after kill-server below.
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if !o.followEnv {
		shimDir := filepath.Join(dir, "bin")
		if err := os.Mkdir(shimDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// The shim refuses to run once the sandbox is gone rather than let
		// tmux create a fresh server somewhere else.
		shim := fmt.Sprintf("#!/bin/sh\n[ -d %s ] || { echo 'tmuxtest: sandbox %s is gone' >&2; exit 97; }\nexec %s -S %s -f /dev/null \"$@\"\n",
			shellQuote(dir), dir, shellQuote(bin), shellQuote(s.Socket))
		if err := os.WriteFile(filepath.Join(shimDir, "tmux"), []byte(shim), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(s.kill)
	return s
}

// Run runs tmux against the sandbox server and returns its trimmed output;
// an error carries tmux's diagnostic. Only new-session may run before the
// socket exists: anything else would have no server to talk to.
func (s *Server) Run(args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("tmuxtest: no tmux command")
	}
	if args[0] != "new-session" && !s.exists() {
		return "", fmt.Errorf("tmuxtest: refusing %q: sandbox socket %s does not exist", args[0], s.Socket)
	}
	cmd := exec.Command(s.bin, append([]string{"-S", s.Socket, "-f", "/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// Start starts the server with a detached new-session given args (after -d).
// A host that cannot run a tmux server skips the test, since nothing about
// tmux placement can be checked there (#446). Two ways have been seen, both
// inside agent sandboxes: the server may not bind its socket ("error creating
// <sock> (Operation not permitted)", yet tmux 3.5a exits 0 and the server is
// gone at once), or a pane may get no terminal ("fork failed: Device not
// configured"). Start checks that the server answers, rather than trusting
// new-session's exit status.
func (s *Server) Start(args ...string) {
	s.t.Helper()
	out, err := s.Run(append([]string{"new-session", "-d"}, args...)...)
	if NoPTY(err) {
		s.t.Skipf("host cannot give a tmux pane a terminal: %v", err)
	}
	if err != nil {
		s.t.Fatalf("start tmux sandbox: %v", err)
	}
	if _, answer := s.Run("has-session"); answer != nil {
		if !s.exists() {
			s.t.Skipf("host cannot run a tmux server (its socket was not created): %s", out)
		}
		s.t.Fatalf("tmux sandbox started but does not answer: %v", answer)
	}
}

// TMUX is the $TMUX value of a client inside this server, for code that
// must believe it runs in one of its panes.
func (s *Server) TMUX() string {
	s.t.Helper()
	out, err := s.Run("display-message", "-p", "#{socket_path},#{pid},0")
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

// NoPTY reports whether err is tmux failing to give a pane a terminal.
func NoPTY(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "fork failed") || strings.Contains(msg, "Device not configured") ||
		strings.Contains(msg, "openpty") || strings.Contains(msg, "posix_openpt")
}

// kill stops the sandbox server, and only it: by its socket path, and only
// while that socket exists.
func (s *Server) kill() {
	if !s.exists() || LiveSockets()[s.Socket] {
		return
	}
	cmd := exec.Command(s.bin, "-S", s.Socket, "kill-server")
	cmd.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=")
	_ = cmd.Run()
}

func (s *Server) exists() bool {
	info, err := os.Lstat(s.Socket)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// LiveSockets are the socket paths a bare tmux client would reach from this
// process's environment: the one in $TMUX, and "default" under $TMUX_TMPDIR
// and the system temp dirs. A sandbox must never be one of them.
func LiveSockets() map[string]bool {
	live := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		live[p] = true
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			live[resolved] = true
		}
	}
	if v := os.Getenv("TMUX"); v != "" {
		add(strings.SplitN(v, ",", 2)[0])
	}
	uid := fmt.Sprintf("tmux-%d", os.Getuid())
	for _, base := range []string{os.Getenv("TMUX_TMPDIR"), "/tmp", "/private/tmp"} {
		if base != "" {
			add(filepath.Join(base, uid, "default"))
		}
	}
	return live
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
