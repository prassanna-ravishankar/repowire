package service

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// tmuxTimeout and processQueryTimeout bound the daemon's tmux and ps calls.
const (
	tmuxTimeout         = 5 * time.Second
	processQueryTimeout = 2 * time.Second
	// boundedWaitDelay caps how long Wait lingers for inherited output pipes
	// after the process group is killed.
	boundedWaitDelay = 500 * time.Millisecond
)

// BoundedCommand builds a command that runs in its own process group; at the
// deadline the whole group is killed, so a grandchild holding an output pipe
// cannot keep the call alive past timeout + boundedWaitDelay.
func BoundedCommand(timeout time.Duration, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = boundedWaitDelay
	return cmd, cancel
}

// TmuxCommand is a tmux invocation bounded by tmuxTimeout.
func TmuxCommand(args ...string) (*exec.Cmd, context.CancelFunc) {
	return BoundedCommand(tmuxTimeout, "tmux", args...)
}

// PSOutput runs ps bounded by processQueryTimeout and returns trimmed stdout.
func PSOutput(args ...string) (string, error) {
	cmd, cancel := BoundedCommand(processQueryTimeout, "ps", args...)
	defer cancel()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
