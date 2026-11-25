package hooks

import (
	"os"
	"os/exec"
	"testing"
)

// The hook reports the agent SessionStart registered while it lives, not the
// pane's first non-shell process, which under a forking wrapper (op run,
// /usr/bin/time) is the wrapper; the daemon refused that pid's claim as a
// twin of the live agent (#542). A gone session pid falls back to the watched
// one.
func TestReportedPIDPrefersTheLiveSessionAgent(t *testing.T) {
	self := os.Getpid()
	if got := reportedPID(self, 4242); got != self {
		t.Fatalf("live session pid: reported %d, want %d", got, self)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if got := reportedPID(gone.Process.Pid, self); got != self {
		t.Fatalf("exited session pid: reported %d, want the watched %d", got, self)
	}
	if got := reportedPID(0, self); got != self {
		t.Fatalf("no session pid: reported %d, want the watched %d", got, self)
	}
}
