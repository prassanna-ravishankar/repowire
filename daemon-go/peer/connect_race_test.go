package peer

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// A peer whose socket attaches while lazy repair probes runtime evidence off
// the lock is not demoted: the demote pass re-checks the socket under the lock.
func TestDemoteDisconnectedRechecksTheSocketAfterTheProbe(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}}
	r, _ := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	pane := "%7"
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w"), PaneID: &pane, Machine: "m", Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	r.WithReconciliation(nil, nil, runtimeProbeFunc(func(*proto.Peer) bool {
		transport.mu.Lock()
		transport.connected[id] = true // the socket attaches mid-probe
		transport.mu.Unlock()
		return false
	}), ExperimentsConfig{}, 0, 0)

	r.LazyRepair(ctx)

	if p, _ := r.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("status = %s, want online", p.Status)
	}
}

// A WebSocket connect registers its peer before attaching the socket; lazy
// repair (which that connect starts) must not demote it in between, and judges
// it by its socket again once attached (#569).
func TestLazyRepairSparesAPeerWhoseSocketIsAttaching(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}}
	r, _ := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{AttachingSocket: true, Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w"), Machine: "m", Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Attaching(id) {
		t.Fatal("want attaching after a socket registration")
	}
	r.LazyRepair(ctx)
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("attaching: status = %s, want online", p.Status)
	}

	r.SocketAttached(id) // the attach failed to stick: no socket after all
	r.repairMu.Lock()
	r.lastRepair = time.Time{}
	r.repairMu.Unlock()
	r.LazyRepair(ctx)
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOffline {
		t.Fatalf("attached without a socket: status = %s, want offline", p.Status)
	}
}
