package peer

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

type runtimeProbeFunc func(*proto.Peer) bool

func (f runtimeProbeFunc) HasRuntimeEvidence(peer *proto.Peer) bool { return f(peer) }

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

func TestRuntimeIdentity_LiveHolderIgnoresRetiredStaleClaim(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	pid := 40036
	holder, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, AgentPID: &pid,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := proto.PeerID("repow-work-deadbeef")
	r.mu.Lock()
	r.retired[stale] = Retirement{At: time.Now().UTC(), Hard: true}
	r.mu.Unlock()

	got, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/oumi"), Machine: "m",
		Role: proto.RoleAgent, AgentPID: &pid, ClaimedPeerID: &stale,
	})
	if err != nil || got != holder {
		t.Fatalf("stale retired claim blocked live runtime: %v got %s want %s", err, got, holder)
	}
	r.mu.RLock()
	_, stillRetired := r.retired[stale]
	r.mu.RUnlock()
	if !stillRetired {
		t.Fatal("ignored stale claim must remain retired")
	}
}

func TestRuntimeIdentity_PrefersActiveHolderOverOfflineDuplicate(t *testing.T) {
	now := time.Now().UTC()
	pid := 40036
	offline := &peerState{state: StateOffline, peer: &proto.Peer{PeerID: "offline", AgentPID: &pid, LastSeen: &now}}
	active := &peerState{state: StateOnline, peer: &proto.Peer{PeerID: "active", AgentPID: &pid, LastSeen: &now}}
	if !runtimeHolderPreferred(active, offline) || runtimeHolderPreferred(offline, active) {
		t.Fatal("active runtime holder must win over an offline duplicate")
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
	// A ws reconnect for session b carries the shared pid and its claimed id
	// but no session metadata: it must land on b, never on a.
	claimed := b
	ws, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent,
		AgentPID: &pid, ClaimedPeerID: &claimed,
	})
	if err != nil || ws != b {
		t.Fatalf("session-less reconnect must defer to its claimed id: %v got %s want %s", err, ws, b)
	}
}

// Session-scoped peers sharing one host process are distinct runtimes: when
// both go stale with the process alive, both keep their evidence.
func TestExclusiveEvidence_SessionScopedSiblingsBothSpared(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, _ := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	pid := 9001
	a, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent, AgentPID: &pid, Metadata: map[string]any{"runtime_session_id": "s-a"}})
	b, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentOpenCode, Path: ptr("/p/x"), Machine: "m", Role: proto.RoleAgent, AgentPID: &pid, Metadata: map[string]any{"runtime_session_id": "s-b"}})
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, fakeProbe{evidence: map[proto.PeerID]bool{a: true, b: true}}, ExperimentsConfig{}, 0, time.Nanosecond)
	r.ConfigureDurations(0, time.Nanosecond)
	old := time.Now().UTC().Add(-time.Hour)
	for _, id := range []proto.PeerID{a, b} {
		_, _ = r.MarkOffline(ctx, id, false)
		r.mu.Lock()
		r.peers[id].peer.LastSeen = &old
		r.mu.Unlock()
	}
	r.reapDangling(ctx)
	if n := r.evictStalePeers(ctx); n != 0 {
		t.Fatalf("evicted %d sibling sessions, want 0", n)
	}
	for _, id := range []proto.PeerID{a, b} {
		if _, ok := r.GetPeer(id); !ok {
			t.Fatalf("sibling session %s lost its evidence", id)
		}
	}
}

// A spared peer that goes fresh and later crosses the cutoff again is a new
// transition and is reported again.
func TestSpareEvent_ReportsAgainAfterPeerWentFresh(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	pid, pane := 31337, "%z"
	id, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/z"), Machine: "m", Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid})
	// A 30-minute eviction age keeps "fresh" (now) and "stale" (an hour ago)
	// unambiguous under the race detector's timing.
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, fakeProbe{evidence: map[proto.PeerID]bool{id: true}}, ExperimentsConfig{}, 0, 30*time.Minute)
	_, _ = r.MarkOffline(ctx, id, false)
	backdate := func() {
		old := time.Now().UTC().Add(-time.Hour)
		r.mu.Lock()
		r.peers[id].peer.LastSeen = &old
		r.mu.Unlock()
	}
	backdate()
	r.evictStalePeers(ctx)
	r.evictStalePeers(ctx)
	now := time.Now().UTC()
	r.mu.Lock()
	r.peers[id].peer.LastSeen = &now // touched: fresh again, no longer stale
	r.mu.Unlock()
	r.evictStalePeers(ctx)
	backdate()
	r.evictStalePeers(ctx)
	if n := len(eventsOfType(store, "offline_peer_still_has_runtime_evidence")); n != 2 {
		t.Fatalf("expected two spare transitions, got %d", n)
	}
}

func TestSpareEvent_EvictPassDoesNotResetReapTransition(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	pid := 31337
	id, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/z"), Machine: "m", Role: proto.RoleAgent, AgentPID: &pid})
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, fakeProbe{evidence: map[proto.PeerID]bool{id: true}}, ExperimentsConfig{}, 0, 2*time.Hour)
	r.ConfigureDurations(0, 30*time.Minute)
	_, _ = r.MarkOffline(ctx, id, false)
	old := time.Now().UTC().Add(-time.Hour)
	r.mu.Lock()
	r.peers[id].peer.LastSeen = &old
	r.mu.Unlock()

	r.reapDangling(ctx)
	r.evictStalePeers(ctx) // not old enough for this pass
	r.reapDangling(ctx)
	if n := len(eventsOfType(store, "offline_peer_still_has_runtime_evidence")); n != 1 {
		t.Fatalf("eviction pass reset reap transition marker: got %d events", n)
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

func TestEvictStalePeers_RechecksExclusiveOwnerAfterProbe(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, _ := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	sharedPID, otherPID := 40036, 1
	ghost, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/a"), Machine: "m", Role: proto.RoleAgent, AgentPID: &sharedPID})
	live, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/b"), Machine: "m", Role: proto.RoleAgent, AgentPID: &otherPID})
	_, _ = r.MarkOffline(ctx, ghost, false)
	old := time.Now().UTC().Add(-time.Hour)
	r.mu.Lock()
	r.peers[ghost].peer.LastSeen = &old
	r.mu.Unlock()

	probe := runtimeProbeFunc(func(*proto.Peer) bool {
		r.mu.Lock()
		r.peers[live].peer.AgentPID = &sharedPID
		r.mu.Unlock()
		return true
	})
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, probe, ExperimentsConfig{}, 0, time.Nanosecond)

	if n := r.evictStalePeers(ctx); n != 1 {
		t.Fatalf("evicted %d, want stale peer after runtime changed owners during probe", n)
	}
	if _, ok := r.GetPeer(ghost); ok {
		t.Fatal("stale peer kept evidence after another peer acquired its runtime")
	}
}

// Two stale ghosts sharing one pid with no live holder: the most recently seen
// keeps the evidence, the duplicate evicts. Covers both the eviction and the
// reap path, and the reap emitter also reports once.
func TestExclusiveEvidence_DuplicateStaleHoldersKeepNewestOnly(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})
	pid, otherPID := 4242, 1
	paneA, paneB := "%a", "%b"
	older, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/a"), Machine: "m", Role: proto.RoleAgent, PaneID: &paneA, AgentPID: &pid})
	newer, _, _ := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/p/b"), Machine: "m", Role: proto.RoleAgent, PaneID: &paneB, AgentPID: &otherPID})
	r.mu.Lock()
	r.peers[newer].peer.AgentPID = &pid // pre-fix fork left two records on one pid
	r.mu.Unlock()

	probe := fakeProbe{evidence: map[proto.PeerID]bool{older: true, newer: true}}
	r.WithReconciliation(newFakeAsks(), &fakeDelivery{}, probe, ExperimentsConfig{}, 0, time.Nanosecond)
	r.ConfigureDurations(0, time.Nanosecond)
	for _, id := range []proto.PeerID{older, newer} {
		if _, err := r.MarkOffline(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
	twoHours, oneHour := time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-time.Hour)
	r.mu.Lock()
	r.peers[older].peer.LastSeen = &twoHours
	r.peers[newer].peer.LastSeen = &oneHour
	r.mu.Unlock()

	r.reapDangling(ctx)
	if _, ok := r.GetPeer(older); ok {
		t.Fatal("older duplicate holder must be reaped")
	}
	if _, ok := r.GetPeer(newer); !ok {
		t.Fatal("most recently seen holder must keep the evidence and be spared")
	}
	r.reapDangling(ctx)
	r.evictStalePeers(ctx)
	r.reapDangling(ctx)
	if n := len(eventsOfType(store, "offline_peer_still_has_runtime_evidence")); n != 1 {
		t.Fatalf("spare must be reported once across reap and evict passes, got %d", n)
	}
}
