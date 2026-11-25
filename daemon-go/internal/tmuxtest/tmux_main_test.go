package tmuxtest

import (
	"os"
	"testing"
)

// TestMain keeps these tests away from the live tmux server too.
func TestMain(m *testing.M) { os.Exit(Main(m)) }
