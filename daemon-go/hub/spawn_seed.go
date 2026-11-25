package hub

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// Spawn-with-message (#452): a spawn's opening message is held until the new
// peer is reachable on its pane, and its connection delivers it (seedConnected,
// once the socket is attached and its queue replayed), never a poll.
//
// A runtime can stop at a first-run prompt before it registers at all: Claude
// Code and Codex both ask whether to trust a folder they have not seen (a new
// worktree), and nothing registers until someone answers; an update notice can
// do the same. So the message is held for spawnSeedHold, not dropped after a
// short wait, and the spawner is told at spawnSeedNotice that the peer has not
// connected and where to look. A held message belongs to the runtime the spawn
// launched (its backend and project path): another runtime that comes up in
// the pane does not get it, and the message is dropped loudly. Every outcome is
// journaled.

var (
	// spawnSeedNotice is when the spawner hears that the peer has not
	// connected yet.
	spawnSeedNotice = 90 * time.Second
	// spawnSeedHold is how long the opening message waits for the peer.
	spawnSeedHold = 30 * time.Minute
)

// spawnSender is who the spawner hears seed notices from. ':' is outside the
// peer-name pattern, so it never resolves to a peer.
const spawnSender = "repowire:spawn"

type heldSeed struct {
	pane, name, from, text string
	backend                proto.AgentType
	path                   string
	until                  time.Time
	notice, expiry         *time.Timer
	stopOnShutdown         func() bool
}

func (s *heldSeed) stop() {
	s.notice.Stop()
	s.expiry.Stop()
	s.stopOnShutdown()
}

// spawnSeeds are the opening messages waiting for their peers, by pane.
type spawnSeeds struct {
	mu     sync.Mutex
	byPane map[string]*heldSeed
}

// take removes seed if it is still the one held for its pane.
func (s *spawnSeeds) take(seed *heldSeed) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byPane[seed.pane] != seed {
		return false
	}
	delete(s.byPane, seed.pane)
	seed.stop()
	return true
}

func (s *spawnSeeds) held(pane string) *heldSeed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPane[pane]
}

// holdSpawnSeed keeps text for the runtime spawned on pane (backend, path). A
// message held earlier for the same pane is replaced.
func (h *Hub) holdSpawnSeed(ctx context.Context, pane, name string, backend proto.AgentType, path, from, text string) {
	seed := &heldSeed{pane: pane, name: name, backend: backend, path: service.NormPath(path), from: from, text: text, until: time.Now().Add(spawnSeedHold)}
	seed.notice = time.AfterFunc(spawnSeedNotice, func() { h.seedNotConnected(ctx, seed) })
	seed.expiry = time.AfterFunc(spawnSeedHold, func() { h.seedExpired(ctx, seed) })
	// A daemon shutting down releases what it holds.
	seed.stopOnShutdown = context.AfterFunc(ctx, func() { h.seeds.take(seed) })
	h.seeds.mu.Lock()
	if h.seeds.byPane == nil {
		h.seeds.byPane = map[string]*heldSeed{}
	}
	if old := h.seeds.byPane[pane]; old != nil {
		old.stop()
	}
	h.seeds.byPane[pane] = seed
	h.seeds.mu.Unlock()
	// The peer may already be on the mesh (a restart registers before this).
	if p, ok := h.ask.reg.GetPeerByPane(pane); ok && p != nil && h.seedReachable(p.PeerID) {
		h.seedConnected(ctx, p.PeerID)
	}
}

// seedContext outlives a connection: the daemon's, when spawning is wired.
func (h *Hub) seedContext() context.Context {
	if h.spawn != nil && h.spawn.ctx != nil {
		return h.spawn.ctx
	}
	return context.Background()
}

// seedReachable reports whether id has a live connection to deliver on.
func (h *Hub) seedReachable(id proto.PeerID) bool {
	return h.transport == nil || h.transport.IsConnected(id)
}

// seedConnected delivers the opening message held for the pane id just
// connected on. HandleWS calls it once the socket is attached and its queued
// deliveries are replayed, so the message goes out live.
func (h *Hub) seedConnected(ctx context.Context, id proto.PeerID) {
	if h.ask == nil {
		return
	}
	p, ok := h.ask.reg.GetPeer(id)
	if !ok || p == nil || p.PaneID == nil || *p.PaneID == "" || p.Status == proto.StatusOffline {
		return
	}
	seed := h.seeds.held(*p.PaneID)
	if seed == nil || !h.seeds.take(seed) {
		return
	}
	if p.Backend != seed.backend || service.NormPath(p.Path) != seed.path {
		h.seedDropped(ctx, seed, fmt.Sprintf("pane %s now runs %s (%s), not the spawned runtime", seed.pane, p.DisplayName, p.Backend))
		return
	}
	go h.deliverHeldSeed(ctx, seed, *p)
}

// deliverHeldSeed sends the opening message as a normal notify from the
// spawner. It bypasses the first-turn gate (it is the seed); everything else
// for that peer still waits behind it.
func (h *Hub) deliverHeldSeed(ctx context.Context, seed *heldSeed, p proto.Peer) {
	res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{
		FromPeer: seed.from, ToPeer: string(p.PeerID), Text: seed.text, BypassCircle: true, SkipSeedGate: true,
	})
	payload := map[string]any{"pane_id": seed.pane, "peer_id": string(p.PeerID), "peer_name": string(p.DisplayName), "from_peer": seed.from}
	if err != nil {
		payload["error"] = err.Error()
		log.Printf("spawn: opening message for %s (%s) failed: %v", p.DisplayName, p.PeerID, err)
		h.ask.reg.AddEvent(ctx, "spawn_seed_failed", payload)
		h.tellSpawner(ctx, seed, fmt.Sprintf("[spawn] the opening message for @%s could not be delivered: %v", p.DisplayName, err))
		return
	}
	payload["delivery_state"] = res.DeliveryState
	h.ask.reg.AddEvent(ctx, "spawn_seed_delivered", payload)
}

// seedNotConnected tells the spawner the peer is not on the mesh yet. The
// message stays held.
func (h *Hub) seedNotConnected(ctx context.Context, seed *heldSeed) {
	if ctx.Err() != nil || h.seeds.held(seed.pane) != seed {
		return
	}
	h.ask.reg.AddEvent(ctx, "spawn_seed_waiting", map[string]any{"pane_id": seed.pane, "display_name": seed.name, "from_peer": seed.from, "held_until": seed.until.UTC().Format(time.RFC3339)})
	h.tellSpawner(ctx, seed, fmt.Sprintf(
		"[spawn] %s is not on the mesh after %s (pane %s). Its runtime is most likely waiting at a first-run prompt there (trust this folder, or an update notice); answer it in the pane. The opening message is held and is delivered when the peer connects, until %s.",
		seed.name, spawnSeedNotice.Round(time.Second), seed.pane, seed.until.Local().Format("15:04")))
}

// seedExpired drops a message whose peer never connected.
func (h *Hub) seedExpired(ctx context.Context, seed *heldSeed) {
	if ctx.Err() != nil || !h.seeds.take(seed) {
		return
	}
	h.seedDropped(ctx, seed, fmt.Sprintf("the peer did not connect within %s", spawnSeedHold))
}

// seedDropped journals a message that will not be delivered and tells the
// spawner.
func (h *Hub) seedDropped(ctx context.Context, seed *heldSeed, why string) {
	log.Printf("spawn: opening message for %s on pane %s dropped: %s", seed.name, seed.pane, why)
	h.ask.reg.AddEvent(ctx, "spawn_seed_failed", map[string]any{"pane_id": seed.pane, "display_name": seed.name, "from_peer": seed.from, "error": why})
	h.tellSpawner(ctx, seed, fmt.Sprintf("[spawn] the opening message for %s was dropped: %s. Send it again once the peer is on the mesh.", seed.name, why))
}

// tellSpawner notifies the spawner, best effort: a spawner that is not a peer
// (the CLI, the dashboard) still has the journal.
func (h *Hub) tellSpawner(ctx context.Context, seed *heldSeed, text string) {
	if seed.from == "" || seed.from == mcpDefaultIdentity {
		return
	}
	if _, err := h.ask.delivery.Notify(ctx, service.NotifyParams{FromPeer: spawnSender, ToPeer: seed.from, Text: text, BypassCircle: true}); err != nil {
		log.Printf("spawn: could not tell %s about %s: %v", seed.from, seed.name, err)
	}
}
