package peer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// --- reconcile test doubles ---

// fakeAsks is an in-memory AskTracker double. Asks are stored by correlation_id;
// the Take/Snapshot methods return StashedAsk projections and the Mark/Forget/
// Evict methods mutate under the lock (mirroring the real tracker's invariant
// that the Ask is mutated only inside tracker locks).
type fakeAsks struct {
	mu        sync.Mutex
	asks      map[string]*StashedAsk
	closed    map[string]string // cid -> reason (MarkPendingReplyDelivered)
	rebound   map[string]proto.PeerID
	forgotten []proto.PeerID
	evictedTo int
	expired   map[string]bool // cids whose created_at < ttl cutoff
}

func newFakeAsks() *fakeAsks {
	return &fakeAsks{
		asks:    map[string]*StashedAsk{},
		closed:  map[string]string{},
		rebound: map[string]proto.PeerID{},
		expired: map[string]bool{},
	}
}

func (f *fakeAsks) add(a StashedAsk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := a
	f.asks[a.CorrelationID] = &cp
}

func (f *fakeAsks) TakePendingRepliesForAsker(asker proto.PeerID) []StashedAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StashedAsk
	for _, a := range f.asks {
		if a.FromPeerID == asker && a.PendingReply != nil {
			out = append(out, *a)
		}
	}
	return out
}

func (f *fakeAsks) TakeOrphanPendingRepliesMatching(t AskerIdentity, live map[proto.PeerID]struct{}) []StashedAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StashedAsk
	for _, a := range f.asks {
		if a.PendingReply == nil || a.AskerIdentity == nil {
			continue
		}
		id := *a.AskerIdentity
		if id != t {
			continue
		}
		if _, isLive := live[a.FromPeerID]; isLive {
			continue
		}
		out = append(out, *a)
	}
	return out
}

func (f *fakeAsks) MarkPendingReplyDelivered(cid string, newFrom *proto.PeerID, reason string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.asks[cid]
	if !ok || a.PendingReply == nil {
		return false
	}
	f.closed[cid] = reason
	if newFrom != nil {
		f.rebound[cid] = *newFrom
		a.FromPeerID = *newFrom
	}
	a.PendingReply = nil
	return true
}

func (f *fakeAsks) SnapshotPendingRepliesForPeer(id proto.PeerID) []StashedAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StashedAsk
	for _, a := range f.asks {
		if a.PendingReply != nil && (a.ToPeerID == id || a.FromPeerID == id) {
			out = append(out, *a)
		}
	}
	return out
}

func (f *fakeAsks) SnapshotExpiredPendingReplies() []StashedAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StashedAsk
	for cid, a := range f.asks {
		if a.PendingReply != nil && f.expired[cid] {
			out = append(out, *a)
		}
	}
	return out
}

func (f *fakeAsks) EvictExpired(includeStashed bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for cid := range f.expired {
		if a, ok := f.asks[cid]; ok {
			if !includeStashed && a.PendingReply != nil {
				continue
			}
			delete(f.asks, cid)
			n++
		}
	}
	f.evictedTo += n
	return n
}

func (f *fakeAsks) ForgetPeer(id proto.PeerID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, id)
	n := 0
	for cid, a := range f.asks {
		if a.ToPeerID == id || a.FromPeerID == id {
			delete(f.asks, cid)
			n++
		}
	}
	return n
}

// fakeDelivery records notify calls and can be told to fail.
type fakeDelivery struct {
	mu    sync.Mutex
	calls []deliveryCall
	err   error
}

type deliveryCall struct {
	from proto.PeerID
	to   proto.PeerID
	text string
}

func (d *fakeDelivery) Notify(_ context.Context, from, to proto.PeerID, text string, _ bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.calls = append(d.calls, deliveryCall{from, to, text})
	return nil
}

// fakeProbe answers HasRuntimeEvidence from a set keyed by peer_id.
type fakeProbe struct{ evidence map[proto.PeerID]bool }

func (p fakeProbe) HasRuntimeEvidence(peer *proto.Peer) bool { return p.evidence[peer.PeerID] }

// pingTransport answers Ping from a per-peer queue of pong maps; IsConnected
// from a set. A nil pong with non-nil err is a ping failure (inconclusive).
type pingTransport struct {
	mu        sync.Mutex
	connected map[proto.PeerID]bool
	pongs     map[proto.PeerID][]map[string]any
}

func (t *pingTransport) IsConnected(id proto.PeerID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected[id]
}
func (t *pingTransport) Close(proto.PeerID) error { return nil }
func (t *pingTransport) Ping(_ context.Context, id proto.PeerID, _ time.Duration) (map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	q := t.pongs[id]
	if len(q) == 0 {
		return map[string]any{}, nil // no info -> inconclusive
	}
	pong := q[0]
	if len(q) > 1 {
		t.pongs[id] = q[1:]
	}
	return pong, nil
}

func newRegistryWith(t *testing.T, transport Transport, live Liveness) (*Registry, *memStore) {
	t.Helper()
	store := newMemStore()
	r, err := NewRegistry(context.Background(), store, live, transport)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, store
}

func eventsOfType(s *memStore, typ string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// --- 1. two-pass redelivery uniqueness gate ---

// waitRedeliveryIdle blocks until no redelivery worker holds the single-flight
// slot for asker (claim-then-release succeeds), so a test can call
// redeliverPendingReplies directly without racing a register/status-triggered one.
func waitRedeliveryIdle(t *testing.T, r *Registry, asker proto.PeerID) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if r.claimRedelivery(asker) {
			r.releaseRedelivery(asker)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("redelivery slot never went idle")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRedeliver_SingleFlight_NoDuplicate proves overlapping redelivery workers
// for the same asker deliver a stash exactly once. Without single-flight, two
// workers snapshot the same un-cleared stash and both send it.
func TestRedeliver_SingleFlight_NoDuplicate(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	asks := newFakeAsks()
	delivery := &fakeDelivery{}
	r.WithReconciliation(asks, delivery, fakeProbe{}, ExperimentsConfig{}, 0, 0)

	asker, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "a", Backend: proto.AgentClaudeCode, Path: ptr("/w"), Machine: "h", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	waitRedeliveryIdle(t, r, asker)

	reply := "ans"
	asks.add(StashedAsk{
		CorrelationID: "ask1", FromPeerID: asker, FromPeerName: "a",
		ToPeerID: "repow-a-answerer", ToPeerName: "answerer", PendingReply: &reply,
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.redeliverPendingReplies(ctx, asker) }()
	}
	wg.Wait()

	delivery.mu.Lock()
	got := len(delivery.calls)
	delivery.mu.Unlock()
	if got != 1 {
		t.Fatalf("single-flight: expected exactly 1 delivery for 1 stash across 20 workers, got %d", got)
	}
}

func TestRedeliverPendingReplies_TwoPass_UniquenessGate(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	asks := newFakeAsks()
	delivery := &fakeDelivery{}
	r.WithReconciliation(asks, delivery, fakeProbe{}, ExperimentsConfig{ACPBrokerClient: true}, 0, 0)

	// Asker comes back under a NEW peer_id (clean takeover): register it.
	asker, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/proj"), Machine: "host1", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register asker: %v", err)
	}

	// Pass-1 stash: keyed to the live asker id directly.
	reply1 := "answer-1"
	asks.add(StashedAsk{
		CorrelationID: "ask-same", FromPeerID: asker, FromPeerName: "asker",
		ToPeerID: "repow-alpha-answerer", ToPeerName: "answerer", PendingReply: &reply1,
	})

	// Pass-2 orphan stash: original asker id is GONE; identity tuple matches the
	// reborn asker. normalizeIdentityPath of "/work/proj" is what's stored.
	reply2 := "answer-2"
	asks.add(StashedAsk{
		CorrelationID: "ask-orphan", FromPeerID: "repow-alpha-deadid", FromPeerName: "asker",
		ToPeerID: "repow-alpha-answerer", ToPeerName: "answerer", PendingReply: &reply2,
		AskerIdentity: &AskerIdentity{
			DisplayName: "proj-claude-code", Circle: "alpha", Backend: proto.AgentClaudeCode,
			Path: normalizeIdentityPath("/work/proj"), Machine: "host1",
		},
	})

	// AllocateAndRegister also schedules an async redelivery; it may run before the
	// stashes above existed (delivering 0) and briefly hold the single-flight slot.
	// Poll the direct call until both passes have delivered. Single-flight + the
	// tracker clearing PendingReply on delivery cap the total at 2, so this
	// converges deterministically and still fails loud on any OVER-delivery.
	var got int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.redeliverPendingReplies(ctx, asker)
		delivery.mu.Lock()
		got = len(delivery.calls)
		delivery.mu.Unlock()
		if got >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got != 2 {
		t.Fatalf("expected 2 redeliveries (pass-1 + pass-2), got %d", got)
	}
	asks.mu.Lock()
	_, sameClosed := asks.closed["ask-same"]
	rebound, orphanRebound := asks.rebound["ask-orphan"]
	asks.mu.Unlock()
	if !sameClosed {
		t.Fatalf("pass-1 ask not marked delivered")
	}
	if !orphanRebound || rebound != asker {
		t.Fatalf("pass-2 ask not rebound to %s (rebound=%v to=%s)", asker, orphanRebound, rebound)
	}

	// Uniqueness gate: register a SECOND live peer with the identical identity
	// tuple. A fresh orphan must now be REFUSED (ambiguous -> never misroute).
	r.UpdateDisplayName(ctx, asker, "shared-name") // detach asker's name first
	idA, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentGemini, Path: ptr("/work/dup"), Machine: "host2", Role: proto.RoleAgent,
	})
	idB, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentGemini, Path: ptr("/work/dup"), Machine: "host2", Role: proto.RoleAgent,
	})
	// Allocation now auto-suffixes the second peer's display_name ("dup-2-gemini")
	// so distinct live peers never collide via the registration path. The
	// uniqueness gate still has to defend against a TRANSIENT duplicate (a
	// reborn peer overlapping a not-yet-evicted ghost, or external rename racing
	// the snapshot), so force both live peers onto the identical tuple directly
	// to exercise the gate's ambiguity refusal.
	r.mu.Lock()
	r.peers[idB].peer.DisplayName = r.peers[idA].peer.DisplayName // both "dup-gemini"
	r.mu.Unlock()
	reply3 := "answer-3"
	asks.add(StashedAsk{
		CorrelationID: "ask-ambiguous", FromPeerID: "repow-beta-deadid", FromPeerName: "asker",
		ToPeerID: "repow-beta-answerer", ToPeerName: "answerer", PendingReply: &reply3,
		AskerIdentity: &AskerIdentity{
			DisplayName: "dup-gemini", Circle: "beta", Backend: proto.AgentGemini,
			Path: normalizeIdentityPath("/work/dup"), Machine: "host2",
		},
	})
	delivery.mu.Lock()
	before := len(delivery.calls)
	delivery.mu.Unlock()

	r.redeliverPendingReplies(ctx, idA)

	delivery.mu.Lock()
	after := len(delivery.calls)
	delivery.mu.Unlock()
	if after != before {
		t.Fatalf("ambiguous tuple was delivered (gate failed): before=%d after=%d", before, after)
	}
	asks.mu.Lock()
	_, ambiguousClosed := asks.closed["ask-ambiguous"]
	asks.mu.Unlock()
	if ambiguousClosed {
		t.Fatalf("ambiguous orphan ask was closed despite uniqueness-gate refusal")
	}
}

// --- 2. three-strike PANE_MISSING terminal demotion ---

func TestDemoteUnsafeConnectedPeers_ThreeStrikes(t *testing.T) {
	ctx := context.Background()
	pane := "%42"
	pid := 9001
	transport := &pingTransport{
		connected: map[proto.PeerID]bool{},
		pongs:     map[proto.PeerID][]map[string]any{},
	}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{9001: true}})
	r.WithReconciliation(nil, nil, fakeProbe{}, ExperimentsConfig{}, 0, 0)

	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &pane, AgentPID: &pid,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	transport.connected[id] = true

	// Each LazyRepair-equivalent call pings once. Queue pane_alive=false thrice.
	// (pingTransport returns the head and keeps the last; queue exact count.)
	transport.pongs[id] = []map[string]any{
		{"pane_alive": false}, {"pane_alive": false}, {"pane_alive": false},
	}

	// Strike 1 + 2: still connected, no demotion.
	for i := 1; i <= 2; i++ {
		if n := r.demoteUnsafeConnectedPeers(ctx); n != 0 {
			t.Fatalf("strike %d: demoted %d, want 0", i, n)
		}
		if p, _ := r.GetPeer(id); p.Status != proto.StatusOnline {
			t.Fatalf("strike %d: status %q, want online", i, p.Status)
		}
	}

	// Strike 3: terminal demotion -> retired + PANE_MISSING contradiction.
	if n := r.demoteUnsafeConnectedPeers(ctx); n != 1 {
		t.Fatalf("strike 3: demoted %d, want 1", n)
	}
	// Terminal offline retires the identity and removes it from the live set's
	// reach: a reclaim without a live agent must now be refused.
	store.mu.Lock()
	_, retired := store.retired[id]
	store.mu.Unlock()
	if !retired {
		t.Fatalf("third strike did not retire the identity")
	}
	contras := eventsOfType(store, "peer_contradiction")
	var sawPaneMissing bool
	for _, e := range contras {
		if e.Payload["code"] == ContradictionPaneMissing {
			sawPaneMissing = true
		}
	}
	if !sawPaneMissing {
		t.Fatalf("no PANE_MISSING contradiction emitted on terminal demotion")
	}
}

// --- 3. evidence-gated evict spares a peer with live runtime evidence ---

func TestEvictStalePeers_EvidenceGate_SparesLivePeer(t *testing.T) {
	ctx := context.Background()
	transport := &pingTransport{connected: map[proto.PeerID]bool{}, pongs: map[proto.PeerID][]map[string]any{}}
	r, store := newRegistryWith(t, transport, fakeLive{alive: map[int]bool{}})

	paneAlive := "%alive"
	paneDead := "%dead"
	pidAlive := 1111
	pidDead := 2222

	// Peer SPARED: has runtime evidence (probe says yes).
	spared, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/a"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &paneAlive, AgentPID: &pidAlive,
	})
	if err != nil {
		t.Fatalf("register spared: %v", err)
	}
	// Peer DOOMED: no runtime evidence.
	doomed, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/b"), Machine: "m",
		Role: proto.RoleAgent, PaneID: &paneDead, AgentPID: &pidDead,
	})
	if err != nil {
		t.Fatalf("register doomed: %v", err)
	}

	probe := fakeProbe{evidence: map[proto.PeerID]bool{spared: true, doomed: false}}
	asks := newFakeAsks()
	// evictMaxAge tiny so both peers are immediately past it once OFFLINE.
	r.WithReconciliation(asks, &fakeDelivery{}, probe, ExperimentsConfig{}, 0, time.Nanosecond)

	// Drive both OFFLINE (non-terminal) and backdate last_seen past the cutoff.
	for _, id := range []proto.PeerID{spared, doomed} {
		if _, err := r.MarkOffline(ctx, id, false); err != nil {
			t.Fatalf("MarkOffline %s: %v", id, err)
		}
	}
	old := time.Now().UTC().Add(-time.Hour)
	r.mu.Lock()
	r.peers[spared].peer.LastSeen = &old
	r.peers[doomed].peer.LastSeen = &old
	r.mu.Unlock()

	// A stash owed by the doomed peer must emit pending_reply_lost before forget.
	reply := "lost-reply"
	asks.add(StashedAsk{
		CorrelationID: "ask-doomed", FromPeerID: doomed, FromPeerName: "asker",
		ToPeerID: "repow-alpha-ans", ToPeerName: "ans", PendingReply: &reply,
	})

	n := r.evictStalePeers(ctx)
	if n != 1 {
		t.Fatalf("evicted %d, want 1 (only the no-evidence peer)", n)
	}
	if _, ok := r.GetPeer(spared); !ok {
		t.Fatalf("peer with live runtime evidence was evicted; must be spared")
	}
	if _, ok := r.GetPeer(doomed); ok {
		t.Fatalf("peer with no runtime evidence survived; must be evicted")
	}

	// Spared peer emits the evidence event; doomed peer emits pending_reply_lost
	// (snapshot -> emit -> forget) before its ask is forgotten.
	if len(eventsOfType(store, "offline_peer_still_has_runtime_evidence")) != 1 {
		t.Fatalf("expected one offline_peer_still_has_runtime_evidence event")
	}
	if len(eventsOfType(store, "pending_reply_lost")) != 1 {
		t.Fatalf("expected one pending_reply_lost event for the doomed stash")
	}
	asks.mu.Lock()
	forgot := append([]proto.PeerID(nil), asks.forgotten...)
	asks.mu.Unlock()
	var forgotDoomed bool
	for _, id := range forgot {
		if id == doomed {
			forgotDoomed = true
		}
	}
	if !forgotDoomed {
		t.Fatalf("doomed peer's asks were not forgotten")
	}
	// Evict does NOT retire (asymmetry vs reap).
	store.mu.Lock()
	_, retired := store.retired[doomed]
	store.mu.Unlock()
	if retired {
		t.Fatalf("evictStalePeers retired %s; evict must delete without retirement", doomed)
	}
}
