package hub

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

type seedRig struct {
	h       *Hub
	reg     *askFakeRegistry
	f       *fakeTransport
	spawner *proto.Peer
}

func newSeedRig(t *testing.T) *seedRig {
	t.Helper()
	spawner := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(spawner)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}, sessions: []proto.PeerID{spawner.PeerID}, connected: map[proto.PeerID]bool{}}
	asks := service.NewAskTracker(0)
	h := &Hub{}
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil), reg)
	return &seedRig{h: h, reg: reg, f: f, spawner: spawner}
}

const (
	seedBackend = proto.AgentClaudeCode
	seedPath    = "/work/repo"
)

// hold holds a seed for the runtime a spawn launched on pane.
func (r *seedRig) hold(ctx context.Context, pane, name, text string) {
	r.h.holdSpawnSeed(ctx, pane, name, seedBackend, seedPath, "alpha", text)
}

// register puts a peer on pane and connects it, as HandleWS would.
func (r *seedRig) register(pane, id, name string, status proto.PeerStatus) *proto.Peer {
	return r.registerAs(pane, id, name, status, seedBackend, seedPath)
}

func (r *seedRig) registerAs(pane, id, name string, status proto.PeerStatus, backend proto.AgentType, path string) *proto.Peer {
	p := peerWith(id, name, "default", status)
	p.PaneID = &pane
	p.Backend, p.Path = backend, path
	p.TurnState = proto.TurnPendingFirstTurn
	r.reg.mu.Lock()
	r.reg.peers = append(r.reg.peers, p)
	r.reg.byPane[pane] = p
	r.reg.mu.Unlock()
	r.f.mu.Lock()
	r.f.sessions = append(r.f.sessions, p.PeerID)
	r.f.mu.Unlock()
	r.h.seedConnected(context.Background(), p.PeerID)
	return p
}

func (r *seedRig) waitEvent(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.reg.mu.Lock()
		for _, e := range r.reg.events {
			if e.typ == typ {
				r.reg.mu.Unlock()
				return e.payload
			}
		}
		r.reg.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event", typ)
	return nil
}

func (r *seedRig) sentTo(id proto.PeerID) int {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	n := 0
	for _, target := range append(append([]proto.PeerID(nil), r.f.sentTargets...), r.f.ackTargets...) {
		if target == id {
			n++
		}
	}
	return n
}

func withSeedTiming(t *testing.T, notice, hold time.Duration) {
	t.Helper()
	prevNotice, prevHold := spawnSeedNotice, spawnSeedHold
	spawnSeedNotice, spawnSeedHold = notice, hold
	t.Cleanup(func() { spawnSeedNotice, spawnSeedHold = prevNotice, prevHold })
}

// The registration delivers the held message; nothing is sent before it.
func TestHeldSeedIsDeliveredByTheRegistration(t *testing.T) {
	withSeedTiming(t, time.Hour, time.Hour)
	r := newSeedRig(t)
	r.hold(context.Background(), "%42", "beta-claude-code", "review PR 400")
	time.Sleep(50 * time.Millisecond)
	r.f.mu.Lock()
	early := len(r.f.sentTargets) + len(r.f.ackTargets)
	r.f.mu.Unlock()
	if early != 0 {
		t.Fatal("notified before the peer registered")
	}
	newcomer := r.register("%42", "repow-default-bbbb", "beta-claude-code", proto.StatusOnline)
	got := r.waitEvent(t, "spawn_seed_delivered")
	if got["peer_id"] != string(newcomer.PeerID) || got["from_peer"] != "alpha" {
		t.Fatalf("spawn_seed_delivered = %v", got)
	}
	if r.sentTo(newcomer.PeerID) != 1 {
		t.Fatalf("seed frames to the newcomer = %d, want 1", r.sentTo(newcomer.PeerID))
	}
	// A reconnect later does not deliver it again.
	r.h.seedConnected(context.Background(), newcomer.PeerID)
	time.Sleep(50 * time.Millisecond)
	if r.sentTo(newcomer.PeerID) != 1 {
		t.Fatal("a reconnect redelivered the opening message")
	}
}

// A peer that registered before the message was held still gets it.
func TestHeldSeedReachesAPeerAlreadyRegistered(t *testing.T) {
	withSeedTiming(t, time.Hour, time.Hour)
	r := newSeedRig(t)
	early := r.register("%43", "repow-default-cccc", "gamma-codex", proto.StatusOnline)
	r.hold(context.Background(), "%43", "gamma-codex", "hello")
	if got := r.waitEvent(t, "spawn_seed_delivered"); got["peer_id"] != string(early.PeerID) {
		t.Fatalf("spawn_seed_delivered = %v", got)
	}
}

// A runtime stopped at a first-run prompt (folder trust) registers late: the
// spawner hears about it, the message stays held, and the late registration
// still delivers it (#452).
func TestSpawnerIsToldAndTheSeedSurvivesALateRegistration(t *testing.T) {
	withSeedTiming(t, 20*time.Millisecond, time.Hour)
	r := newSeedRig(t)
	r.hold(context.Background(), "%44", "delta-codex", "the brief")
	waiting := r.waitEvent(t, "spawn_seed_waiting")
	if waiting["pane_id"] != "%44" {
		t.Fatalf("spawn_seed_waiting = %v", waiting)
	}
	deadline := time.Now().Add(time.Second)
	for r.sentTo(r.spawner.PeerID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.sentTo(r.spawner.PeerID) != 1 {
		t.Fatalf("notices to the spawner = %d, want 1", r.sentTo(r.spawner.PeerID))
	}
	late := r.register("%44", "repow-default-dddd", "delta-codex", proto.StatusOnline)
	if got := r.waitEvent(t, "spawn_seed_delivered"); got["peer_id"] != string(late.PeerID) {
		t.Fatalf("spawn_seed_delivered = %v", got)
	}
}

// An offline registration (a pane handoff) does not consume the message.
func TestOfflineRegistrationKeepsTheSeedHeld(t *testing.T) {
	withSeedTiming(t, time.Hour, time.Hour)
	r := newSeedRig(t)
	r.hold(context.Background(), "%45", "eps", "hello")
	r.register("%45", "repow-default-eeee", "eps", proto.StatusOffline)
	if r.h.seeds.held("%45") == nil {
		t.Fatal("an offline registration consumed the opening message")
	}
}

// A message whose peer never registers is dropped loudly: journaled, and the
// spawner told; a registration afterwards gets nothing.
func TestHeldSeedExpiresLoudly(t *testing.T) {
	withSeedTiming(t, time.Hour, 20*time.Millisecond)
	r := newSeedRig(t)
	r.hold(context.Background(), "%99", "ghost", "hello")
	if got := r.waitEvent(t, "spawn_seed_failed"); got["pane_id"] != "%99" {
		t.Fatalf("spawn_seed_failed = %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for r.sentTo(r.spawner.PeerID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.sentTo(r.spawner.PeerID) != 1 {
		t.Fatal("the spawner was not told the message was dropped")
	}
	ghost := r.register("%99", "repow-default-ffff", "ghost", proto.StatusOnline)
	time.Sleep(50 * time.Millisecond)
	if r.sentTo(ghost.PeerID) != 0 {
		t.Fatal("an expired message was delivered")
	}
}

// Shutdown silences the timers.
func TestHeldSeedTimersStopWithTheDaemon(t *testing.T) {
	withSeedTiming(t, 10*time.Millisecond, 20*time.Millisecond)
	r := newSeedRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.hold(ctx, "%46", "zeta", "hello")
	cancel()
	time.Sleep(80 * time.Millisecond)
	r.reg.mu.Lock()
	n := len(r.reg.events)
	r.reg.mu.Unlock()
	if n != 0 || r.sentTo(r.spawner.PeerID) != 0 {
		t.Fatalf("timers acted after shutdown: %d events", n)
	}
	if r.h.seeds.held("%46") != nil {
		t.Fatal("shutdown left the message held")
	}
}

// Another runtime that comes up in the pane first (a shell wrapper, an
// unrelated agent) does not get the spawn's opening message; it stays held for
// the spawned runtime, which gets it when it connects.
func TestAnotherRuntimeOnThePaneDoesNotGetTheSeed(t *testing.T) {
	withSeedTiming(t, time.Hour, time.Hour)
	r := newSeedRig(t)
	r.hold(context.Background(), "%47", "eta", "the brief")
	other := r.registerAs("%47", "repow-default-9999", "other-codex", proto.StatusOnline, proto.AgentCodex, "/work/elsewhere")
	if got := r.waitEvent(t, "spawn_seed_held"); got["other_peer_id"] != string(other.PeerID) {
		t.Fatalf("spawn_seed_held = %v", got)
	}
	if r.sentTo(other.PeerID) != 0 || r.h.seeds.held("%47") == nil {
		t.Fatal("another runtime took the spawn's opening message")
	}
	spawned := r.register("%47", "repow-default-8888", "eta", proto.StatusOnline)
	if got := r.waitEvent(t, "spawn_seed_delivered"); got["peer_id"] != string(spawned.PeerID) {
		t.Fatalf("spawn_seed_delivered = %v", got)
	}
}

// A newer spawn on the same pane replaces the held message loudly.
func TestReplacedSeedIsJournaledAndTheSpawnerTold(t *testing.T) {
	withSeedTiming(t, time.Hour, time.Hour)
	r := newSeedRig(t)
	r.hold(context.Background(), "%48", "theta", "first brief")
	r.hold(context.Background(), "%48", "theta", "second brief")
	got := r.waitEvent(t, "spawn_seed_failed")
	if got["pane_id"] != "%48" {
		t.Fatalf("spawn_seed_failed = %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for r.sentTo(r.spawner.PeerID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.sentTo(r.spawner.PeerID) != 1 {
		t.Fatal("the spawner was not told its first message was replaced")
	}
}
