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
	started, ok := ProcessStartTime(self)
	if !ok {
		t.Fatal("no start time for this process")
	}
	if got := reportedPID(agentProcess{pid: self, started: started}, 4242); got != self {
		t.Fatalf("live session pid: reported %d, want %d", got, self)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if got := reportedPID(agentProcess{pid: gone.Process.Pid}, self); got != self {
		t.Fatalf("exited session pid: reported %d, want the watched %d", got, self)
	}
	if got := reportedPID(agentProcess{}, self); got != self {
		t.Fatalf("no session pid: reported %d, want the watched %d", got, self)
	}
	// A live pid whose start time differs is a recycled pid, not the agent.
	if got := reportedPID(agentProcess{pid: self, started: "Thu Jan  1 00:00:00 1970"}, 4242); got != 4242 {
		t.Fatalf("recycled session pid: reported %d, want the watched 4242", got)
	}
}
