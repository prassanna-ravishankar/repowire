package hub

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// The spawn-with-message contract: wait for the new peer to appear on its pane,
// then notify it from the spawner. Registration lands mid-wait here.
func TestDeliverSpawnSeedWaitsForRegistrationThenNotifies(t *testing.T) {
	spawner := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(spawner)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}, sessions: []proto.PeerID{spawner.PeerID}, connected: map[proto.PeerID]bool{}}
	asks := service.NewAskTracker(0)
	h := &Hub{}
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil), reg)

	done := make(chan struct{})
	go func() {
		h.deliverSpawnSeed(context.Background(), "%42", "beta-claude-code", "alpha", "review PR 400")
		close(done)
	}()
	time.Sleep(400 * time.Millisecond) // the pane is empty: nothing may be sent yet
	if len(f.sentTargets) != 0 {
		t.Fatalf("notified before the peer registered: %v", f.sentTargets)
	}
	newcomer := peerWith("repow-default-bbbb", "beta-claude-code", "default", proto.StatusOnline)
	newcomer.TurnState = proto.TurnPendingFirstTurn
	reg.mu.Lock()
	reg.peers = append(reg.peers, newcomer)
	reg.byPane["%42"] = newcomer
	reg.mu.Unlock()
	f.mu.Lock()
	f.sessions = append(f.sessions, newcomer.PeerID)
	f.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("seed delivery did not complete after registration")
	}
	reg.mu.Lock()
	last := reg.events[len(reg.events)-1]
	reg.mu.Unlock()
	if last.typ != "spawn_seed_delivered" || last.payload["peer_id"] != string(newcomer.PeerID) || last.payload["from_peer"] != "alpha" {
		t.Fatalf("expected spawn_seed_delivered to the newcomer from alpha, got %s %v", last.typ, last.payload)
	}
	f.mu.Lock()
	target := f.lastTarget
	f.mu.Unlock()
	if target != newcomer.PeerID {
		t.Fatalf("seed frame went to %s, want %s", target, newcomer.PeerID)
	}
}

// A peer that never registers is reported, not silently dropped.
func TestDeliverSpawnSeedJournalsTimeout(t *testing.T) {
	reg := newAskFakeRegistry(peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline))
	f := &fakeTransport{connected: map[proto.PeerID]bool{}}
	asks := service.NewAskTracker(0)
	h := &Hub{}
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil), reg)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	h.deliverSpawnSeed(ctx, "%99", "ghost", "alpha", "hello")
	// ctx expiry returns without a journal entry; the real timeout path journals.
	// Exercise the timeout branch directly with a near-zero deadline.
	old := spawnSeedTimeoutForTest(time.Millisecond)
	defer old()
	h.deliverSpawnSeed(context.Background(), "%99", "ghost", "alpha", "hello")
	if got := lastEventType(reg); got != "spawn_seed_failed" {
		t.Fatalf("expected spawn_seed_failed journal event, got %q", got)
	}
}

func lastEventType(r *askFakeRegistry) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return ""
	}
	return r.events[len(r.events)-1].typ
}

func spawnSeedTimeoutForTest(d time.Duration) func() {
	prev := spawnSeedTimeout
	spawnSeedTimeout = d
	return func() { spawnSeedTimeout = prev }
}
