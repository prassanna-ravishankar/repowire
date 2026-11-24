package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// A pass added while wiring runs on every lazy repair; the retention sweeper
// hangs off it.
func TestLazyRepairRunsAddedPasses(t *testing.T) {
	r, _ := newRegistryWith(t, &pingTransport{connected: map[proto.PeerID]bool{}}, fakeLive{alive: map[int]bool{}})
	runs := 0
	r.AddRepairPass(func(context.Context) { runs++ })
	r.LazyRepair(context.Background())
	if runs != 1 {
		t.Fatalf("pass ran %d times, want 1", runs)
	}
}
