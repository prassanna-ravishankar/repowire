package peer

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// One Claude process registers twice: first the MCP stdio proxy (pid + pane, no
// hook session), then SessionStart (pid + pane + hook session). Both must land
// on one peer with the base name; nothing gets suffixed or displaced.
func TestRuntimeIdentity_SameProcessConvergesOnOnePeer(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	pid, pane := 40036, "%105"

	first, firstName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, secondName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid,
		Metadata: map[string]any{"hook_session_id": "sess-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || firstName != secondName || string(firstName) != "oumi-claude-code" {
		t.Fatalf("same process forked identity: %s/%s vs %s/%s", first, firstName, second, secondName)
	}
	r.mu.RLock()
	count := len(r.peers)
	r.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected one peer, got %d", count)
	}
	if n := len(eventsOfType(store, "peer_offline")); n != 0 {
		t.Fatalf("convergence must not displace anyone, got %d peer_offline", n)
	}
	// A /clear later: new hook session, same process, still the same peer.
	third, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid,
		Metadata: map[string]any{"hook_session_id": "sess-2"},
	})
	if err != nil || third != first {
		t.Fatalf("process-scoped backend must survive a session change: %v %s", err, third)
	}
	// A stale peer_id claim loses to the live pid.
	stale := proto.PeerID("repow-work-deadbeef")
	fourth, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid, ClaimedPeerID: &stale,
	})
	if err != nil || fourth != first {
		t.Fatalf("stale claim should be ignored in favour of the live pid: %v %s", err, fourth)
	}
}

// Session-scoped bridges host many sessions in one process: same pid, distinct
// runtime sessions, distinct peers. A registration without a session id joins
// the existing one rather than minting.
func TestRuntimeIdentity_SessionScopedBackendKeepsSessionsApart(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	pid := 777
	a, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent,
		AgentPID: &pid, Metadata: map[string]any{"runtime_session_id": "s-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, bName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent,
		AgentPID: &pid, Metadata: map[string]any{"runtime_session_id": "s-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a == b || string(bName) != "x-2-opencode" {
		t.Fatalf("distinct sessions in one process must stay distinct: %s %s (%s)", a, b, bName)
	}
	again, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent,
		AgentPID: &pid, Metadata: map[string]any{"runtime_session_id": "s-a"},
	})
	if err != nil || again != a {
		t.Fatalf("same session must reconnect to its own peer: %v %s", err, again)
	}
}

// A different process in the same pane and path is a real newcomer.
func TestRuntimeIdentity_DifferentPidIsDifferentPeer(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	pane := "%9"
	p1, p2 := 100, 200
	a, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p"), Machine: "m", Role: proto.RoleAgent, PaneID: &pane, AgentPID: &p1})
	b, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p"), Machine: "m", Role: proto.RoleAgent, PaneID: &pane, AgentPID: &p2})
	if a == b {
		t.Fatal("different pids must not share a peer")
	}
}

// Pre-existing ghosts: an offline record whose pid belongs to another registered
// peer has no evidence of its own and is evicted, and a genuine spare is
// reported once, not on every pass.
func TestEvictStalePeers_SharedPidIsNotEvidenceAndSpareReportsOnce(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	sharedPID, lonePID := 40036, 555
	paneLive, paneGhost, paneLone := "%1", "%2", "%3"

	ghost, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/a"), Machine: "m", Role: proto.RoleAgent, PaneID: &paneGhost, AgentPID: &sharedPID})
	// The live twin registers as a different process first, then adopts the pid
	// (simulates the pre-fix fork; post-fix the ghost could not exist).
	otherPID := 1
	live, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/b"), Machine: "m", Role: proto.RoleAgent, PaneID: &paneLive, AgentPID: &otherPID})
	lone, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/c"), Machine: "m", Role: proto.RoleAgent, PaneID: &paneLone, AgentPID: &lonePID})
	r.mu.Lock()
	r.peers[live].peer.AgentPID = &sharedPID
	r.mu.Unlock()

	probe := fakeProbe{evidence: map[proto.PeerID]bool{ghost: true, lone: true}}
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, probe, ExperimentsConfig{}, 0, time.Nanosecond)
	for _, id := range []proto.PeerID{ghost, lone} {
		if _, err := r.MarkOffline(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().Add(-time.Hour)
	r.mu.Lock()
	r.peers[ghost].peer.LastSeen = &old
	r.peers[lone].peer.LastSeen = &old
	r.mu.Unlock()

	if n := r.evictStalePeers(ctx); n != 1 {
		t.Fatalf("evicted %d, want 1 (the ghost sharing a live peer's pid)", n)
	}
	if _, ok := r.GetPeer(ghost); ok {
		t.Fatal("ghost sharing another peer's pid must be evicted")
	}
	if _, ok := r.GetPeer(lone); !ok {
		t.Fatal("peer with exclusive live evidence must be spared")
	}
	r.evictStalePeers(ctx)
	r.evictStalePeers(ctx)
	if n := len(eventsOfType(store, "offline_peer_still_has_runtime_evidence")); n != 1 {
		t.Fatalf("spare event must fire once per transition, got %d", n)
	}
}
