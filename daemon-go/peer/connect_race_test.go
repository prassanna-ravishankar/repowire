package peer

import (
	"context"
	"testing"

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

// RestoreConnected brings back a peer that was demoted before its socket was
// attached, and leaves a peer with no socket offline.
func TestRestoreConnectedUndoesADemotionBeforeAttach(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}}
	r, _ := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w"), Machine: "m", Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	r.LazyRepair(ctx) // no socket yet: demoted
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOffline {
		t.Fatalf("precondition: status = %s, want offline", p.Status)
	}
	r.RestoreConnected(ctx, id)
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOffline {
		t.Fatalf("no socket: status = %s, want offline", p.Status)
	}
	transport.mu.Lock()
	transport.connected[id] = true
	transport.mu.Unlock()
	r.RestoreConnected(ctx, id)
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("socket attached: status = %s, want online", p.Status)
	}
}
