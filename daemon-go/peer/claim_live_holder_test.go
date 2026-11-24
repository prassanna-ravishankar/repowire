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

// The sticky-orchestrator step reuses the orchestrator's identity for a
// same-backend registrant on its pane; a twin run by another live process must
// not get it that way either.
func TestStickyOrchestratorIsNotSharedWithALiveTwin(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	p1, p2 := 2001, 2002
	r.live = fakeLive{alive: map[int]bool{p1: true, p2: true}}
	params := func(pid int, claim *proto.PeerID) AllocateParams {
		return AllocateParams{ClaimedPeerID: claim, Circle: "1", Backend: proto.AgentClaudeCode, Path: ptr("/orch"),
			Machine: "m", Role: proto.RoleOrchestrator, AgentPID: &pid, PaneID: ptr("%o")}
	}
	x, _, err := r.AllocateAndRegister(ctx, params(p1, nil))
	if err != nil {
		t.Fatal(err)
	}
	twin, _, err := r.AllocateAndRegister(ctx, params(p2, &x))
	if err != nil {
		t.Fatal(err)
	}
	if twin == x {
		t.Fatalf("a live twin (pid %d) took the sticky orchestrator %s held by pid %d", p2, x, p1)
	}
	if held, _ := r.GetPeer(x); held == nil || *held.AgentPID != p1 || held.PaneID == nil || *held.PaneID != "%o" {
		t.Fatalf("orchestrator = %+v; it must keep its pane and pid", held)
	}
	if again, _, err := r.AllocateAndRegister(ctx, params(p1, &x)); err != nil || again != x {
		t.Fatalf("the orchestrator's own reconnect = %s, %v; want %s", again, err, x)
	}
}

// After a daemon restart the twins re-claim x through the persisted mapping;
// whichever reconnects first keeps it and the other is separated.
func TestTwinsSeparateAfterDaemonRestart(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	p1, p2 := 3001, 3002
	alive := fakeLive{alive: map[int]bool{p1: true, p2: true}}
	r.live = alive
	params := func(pid int, pane string, claim *proto.PeerID) AllocateParams {
		return AllocateParams{ClaimedPeerID: claim, Circle: "1", Backend: proto.AgentClaudeCode, Path: ptr("/work/repo"),
			Machine: "m", Role: proto.RoleAgent, AgentPID: &pid, PaneID: ptr(pane)}
	}
	x, _, err := r.AllocateAndRegister(ctx, params(p1, "%a", nil))
	if err != nil {
		t.Fatal(err)
	}
	r.LazyRepair(ctx) // persist the mapping

	restarted, err := NewRegistry(ctx, store, alive, fakeTransport{connected: map[proto.PeerID]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := restarted.AllocateAndRegister(ctx, params(p2, "%b", &x))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := restarted.AllocateAndRegister(ctx, params(p1, "%a", &x))
	if err != nil {
		t.Fatal(err)
	}
	if first != x {
		t.Fatalf("first reconnect = %s; want %s reclaimed through the persisted mapping", first, x)
	}
	if second == x {
		t.Fatalf("both twins share %s after a daemon restart", x)
	}
}
