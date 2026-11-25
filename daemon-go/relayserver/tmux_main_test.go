package relayserver

import (
	"os"
	"testing"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// TestMain keeps every test in this package away from the live tmux server.
func TestMain(m *testing.M) { os.Exit(tmuxtest.Main(m)) }
