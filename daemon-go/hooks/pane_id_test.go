package hooks

import (
	"testing"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// A hook with $TMUX set but no TMUX_PANE, outside every pane's process tree,
// must not claim a pane. An untargeted display-message answered with the
// session's active pane, binding the peer to another window (#647).
func TestGetPaneIDOutsideEveryPaneFailsClosed(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	srv.Start("-s", "probe", "sleep 30")
	t.Setenv("TMUX", srv.TMUX())
	t.Setenv("TMUX_PANE", "")
	if active, err := srv.Run("display-message", "-p", "-t", "probe:", "#{pane_id}"); err != nil || active == "" {
		t.Fatalf("active pane: %q %v", active, err)
	}
	if pane := getPaneID(); pane != "" {
		t.Fatalf("getPaneID() = %q, want \"\": this process runs in no pane", pane)
	}
}
