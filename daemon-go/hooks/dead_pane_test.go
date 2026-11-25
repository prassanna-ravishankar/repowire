package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// A ws-hook whose pane is gone must not adopt an agent from elsewhere on the
// machine. tmux reports no pid for a missing pane; searching the "pane tree"
// from pid 0 walked every process and returned the first one with the agent's
// command, so an orphaned hook reconnected as another live session.
func TestGonePaneHasNoAgent(t *testing.T) {
	tmuxtest.New(t, tmuxtest.FollowEnv()) // never started: every pane is gone
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	// A command certainly running on this machine, normalized as processSnapshot does.
	self := strings.ToLower(filepath.Base(strings.TrimSpace(string(out))))

	if pid := findExpectedAgentPID("%9999", self); pid != 0 {
		t.Fatalf("findExpectedAgentPID on a gone pane = %d; it adopted a process outside the pane", pid)
	}
	if cmd, pid := capturePaneBaseline("%9999"); pid != 0 {
		t.Fatalf("capturePaneBaseline on a gone pane = %q/%d", cmd, pid)
	}
	if safe := paneSafe("%9999", self); safe == nil || *safe {
		t.Fatalf("paneSafe on a gone pane = %v, want false", safe)
	}
}

func TestPaneTreeFindStaysInTheTree(t *testing.T) {
	children := map[int][]int{0: {1}, 1: {10, 20}, 10: {11}}
	commands := map[int]string{1: "launchd", 10: "zsh", 11: "claude", 20: "claude"}
	isClaude := func(cmd string) bool { return cmd == "claude" }
	if _, pid := paneTreeFind(10, children, commands, isClaude); pid != 11 {
		t.Fatalf("pid = %d, want 11 (the agent under the pane's shell)", pid)
	}
	if _, pid := paneTreeFind(0, children, commands, isClaude); pid != 0 {
		t.Fatalf("an unknown root searched the whole process table: %d", pid)
	}
}

// tmux 3.5 answers display-message for a missing pane with empty formats and
// exit 0, as if the probe were inconclusive. With a server running, a pane that
// tmux does not echo back is gone, and its hook must see that and exit.
func TestMissingPaneOnALiveServerIsGone(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	srv.Start("-s", "probe", "sleep 30")
	// Hooks run inside a pane, with TMUX naming the server; that is when tmux
	// answers a missing target with empty formats instead of an error.
	t.Setenv("TMUX", srv.TMUX())
	live, err := srv.Run("display-message", "-p", "-t", "probe:", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := panePID(live); !ok || pid <= 0 {
		t.Fatalf("live pane: pid=%d ok=%v", pid, ok)
	}
	if pid, ok := panePID("%9999"); !ok || pid != 0 {
		t.Fatalf("missing pane on a live server: pid=%d conclusive=%v, want 0, conclusive", pid, ok)
	}
	if safe := paneSafe("%9999", "sleep"); safe == nil || *safe {
		t.Fatalf("paneSafe on a missing pane = %v, want false so the hook exits", safe)
	}
}
