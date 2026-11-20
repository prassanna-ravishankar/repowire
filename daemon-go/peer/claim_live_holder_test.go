package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// #503: a claimed peer_id is not reused while a different live process holds
// it. Twin sessions whose ws-hooks claim one id otherwise flip-flop the peer
// between their panes forever.
func TestClaimHeldByAnotherLiveProcessMintsFreshID(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	p1, p2 := 1001, 1002
	r.live = fakeLive{alive: map[int]bool{p1: true, p2: true}}
	params := func(pid int, pane string, claim *proto.PeerID) AllocateParams {
		return AllocateParams{ClaimedPeerID: claim, Circle: "1", Backend: proto.AgentClaudeCode, Path: ptr("/work/repo"),
			Machine: "m", Role: proto.RoleAgent, AgentPID: &pid, PaneID: ptr(pane)}
	}
	x, _, err := r.AllocateAndRegister(ctx, params(p1, "%a", nil))
	if err != nil {
		t.Fatal(err)
	}

	twin, _, err := r.AllocateAndRegister(ctx, params(p2, "%b", &x))
	if err != nil {
		t.Fatal(err)
	}
	if twin == x {
		t.Fatalf("a claim from live pid %d took over %s, held by live pid %d", p2, x, p1)
	}
	if held, _ := r.GetPeer(x); held == nil || held.PaneID == nil || *held.PaneID != "%a" || *held.AgentPID != p1 {
		t.Fatalf("holder = %+v; it must keep its pane and pid", held)
	}
	store.mu.Lock()
	rejected := false
	for _, e := range store.events {
		rejected = rejected || (e.Type == "peer_id_claim_rejected" && e.PeerID == x)
	}
	store.mu.Unlock()
	if !rejected {
		t.Fatal("no peer_id_claim_rejected event journaled")
	}

	// The twin's hook keeps claiming x on every reconnect; it must converge on
	// its own peer, not mint another each time.
	if again, _, err := r.AllocateAndRegister(ctx, params(p2, "%b", &x)); err != nil || again != twin {
		t.Fatalf("the twin's reconnect = %s, %v; want its own %s", again, err, twin)
	}
	if again, _, err := r.AllocateAndRegister(ctx, params(p1, "%a", &x)); err != nil || again != x {
		t.Fatalf("the holder's own reconnect = %s, %v; want %s", again, err, x)
	}
	third := 1003
	r.live = fakeLive{alive: map[int]bool{third: true}}
	if next, _, err := r.AllocateAndRegister(ctx, params(third, "%a", &x)); err != nil || next != x {
		t.Fatalf("a claim after the holder's process died = %s, %v; want reuse of %s", next, err, x)
	}
}
