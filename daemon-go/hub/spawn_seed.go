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
	old := h.seeds.byPane[pane]
	if old != nil {
		old.stop()
	}
	h.seeds.byPane[pane] = seed
	h.seeds.mu.Unlock()
	if old != nil {
		go h.seedDropped(ctx, old, fmt.Sprintf("replaced by a newer spawn on pane %s", pane))
	}
	// The peer may already be on the mesh (a restart registers before this).
	if p, ok := h.ask.reg.GetPeerByPane(pane); ok && p != nil && h.seedReachable(p.PeerID) {
		go h.seedConnected(ctx, p.PeerID)
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

// seedFor returns the opening message held for id's pane when id is the
// runtime the spawn launched there (its backend and project path), and that
// pane's message otherwise; ok reports a match. Another runtime that comes up
// in the pane first (a shell wrapper, an unrelated agent) does not get it; the
// message stays held for the spawned one.
func (h *Hub) seedFor(id proto.PeerID) (p *proto.Peer, seed *heldSeed, ok bool) {
	if h.ask == nil {
		return nil, nil, false
	}
	p, found := h.ask.reg.GetPeer(id)
	if !found || p == nil || p.PaneID == nil || *p.PaneID == "" || p.Status == proto.StatusOffline {
		return nil, nil, false
	}
	seed = h.seeds.held(*p.PaneID)
	if seed == nil {
		return p, nil, false
	}
	return p, seed, p.Backend == seed.backend && service.NormPath(p.Path) == seed.path
}

// seedHeldFor reports whether a connect by id will deliver an opening
// message.
func (h *Hub) seedHeldFor(id proto.PeerID) bool {
	_, _, ok := h.seedFor(id)
	return ok
}

// seedConnected delivers the opening message held for the runtime id, and
// returns once it is sent. HandleWS calls it once the socket is attached,
// before the queued deliveries replay, so the message goes out live and first.
func (h *Hub) seedConnected(ctx context.Context, id proto.PeerID) {
	p, seed, ok := h.seedFor(id)
	if seed == nil {
		return
	}
	if !ok {
		h.ask.reg.AddEvent(ctx, "spawn_seed_held", map[string]any{"pane_id": seed.pane, "display_name": seed.name,
			"other_peer_id": string(p.PeerID), "other_peer_name": string(p.DisplayName), "other_backend": string(p.Backend),
			"reason": "another runtime connected on the pane; the message stays held for the spawned one"})
		return
	}
	if !h.seeds.take(seed) {
		return
	}
	h.deliverHeldSeed(ctx, seed, *p)
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
		"[spawn] %s is not on the mesh after %s (pane %s). Its runtime is most likely waiting at a first-run prompt there (trust this folder, or an update notice); answer it in the pane. The opening message is held and is delivered when the peer connects, until %s (in %s).",
		seed.name, spawnSeedNotice.Round(time.Second), seed.pane, seed.until.UTC().Format("15:04 UTC"), time.Until(seed.until).Round(time.Minute)))
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
