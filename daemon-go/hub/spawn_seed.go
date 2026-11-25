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
// peer registers on its pane, and the registration delivers it
// (ObserveRegistration, on the registry's OnRegistered), never a poll.
//
// A runtime can stop at a first-run prompt before it registers at all: Claude
// Code and Codex both ask whether to trust a folder they have not seen (a new
// worktree), and nothing registers until someone answers; an update notice can
// do the same. So the message is held for spawnSeedHold, not dropped after a
// short wait, and the spawner is told at spawnSeedNotice that the peer has not
// registered and where to look. Every outcome is journaled.

var (
	// spawnSeedNotice is when the spawner hears that the peer has not
	// registered yet.
	spawnSeedNotice = 90 * time.Second
	// spawnSeedHold is how long the opening message waits for registration.
	spawnSeedHold = 30 * time.Minute
)

// spawnSender is who the spawner hears seed notices from. ':' is outside the
// peer-name pattern, so it never resolves to a peer.
const spawnSender = "repowire:spawn"

type heldSeed struct {
	pane, name, from, text string
	until                  time.Time
	notice, expiry         *time.Timer
}

// spawnSeeds are the opening messages waiting for their peers, by pane.
type spawnSeeds struct {
	mu     sync.Mutex
	byPane map[string]*heldSeed
}

// take removes and returns the seed held for pane, stopping its timers.
func (s *spawnSeeds) take(pane string) *heldSeed {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed := s.byPane[pane]
	if seed == nil {
		return nil
	}
	delete(s.byPane, pane)
	seed.notice.Stop()
	seed.expiry.Stop()
	return seed
}

// holdSpawnSeed keeps text for the peer that will register on pane. A seed
// held earlier for the same pane is replaced.
func (h *Hub) holdSpawnSeed(ctx context.Context, pane, name, from, text string) {
	seed := &heldSeed{pane: pane, name: name, from: from, text: text, until: time.Now().Add(spawnSeedHold)}
	seed.notice = time.AfterFunc(spawnSeedNotice, func() { h.seedNotRegistered(ctx, seed) })
	seed.expiry = time.AfterFunc(spawnSeedHold, func() { h.seedExpired(ctx, seed) })
	h.seeds.mu.Lock()
	if h.seeds.byPane == nil {
		h.seeds.byPane = map[string]*heldSeed{}
	}
	if old := h.seeds.byPane[pane]; old != nil {
		old.notice.Stop()
		old.expiry.Stop()
	}
	h.seeds.byPane[pane] = seed
	h.seeds.mu.Unlock()
	// The peer may have registered before the seed was held.
	if p, ok := h.ask.reg.GetPeerByPane(pane); ok && p != nil {
		h.ObserveRegistration(*p)
	}
}

// ObserveRegistration delivers the opening message held for the pane p just
// registered on. It never blocks the registration.
func (h *Hub) ObserveRegistration(p proto.Peer) {
	if p.PaneID == nil || *p.PaneID == "" || p.Status == proto.StatusOffline || h.ask == nil {
		return
	}
	seed := h.seeds.take(*p.PaneID)
	if seed == nil {
		return
	}
	ctx := context.Background()
	if h.spawn != nil && h.spawn.ctx != nil {
		ctx = h.spawn.ctx
	}
	go h.deliverHeldSeed(ctx, seed, p)
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

// seedNotRegistered tells the spawner the peer has not registered yet. The
// message stays held.
func (h *Hub) seedNotRegistered(ctx context.Context, seed *heldSeed) {
	if ctx.Err() != nil {
		return
	}
	h.seeds.mu.Lock()
	held := h.seeds.byPane[seed.pane] == seed
	h.seeds.mu.Unlock()
	if !held {
		return
	}
	h.ask.reg.AddEvent(ctx, "spawn_seed_waiting", map[string]any{"pane_id": seed.pane, "display_name": seed.name, "from_peer": seed.from, "held_until": seed.until.UTC().Format(time.RFC3339)})
	h.tellSpawner(ctx, seed, fmt.Sprintf(
		"[spawn] %s has not registered on pane %s after %s. Its runtime is most likely waiting at a first-run prompt there (trust this folder, or an update notice); answer it in the pane. The opening message is held and is delivered when the peer registers, until %s.",
		seed.name, seed.pane, spawnSeedNotice.Round(time.Second), seed.until.Local().Format("15:04")))
}

// seedExpired drops a message whose peer never registered, and says so.
func (h *Hub) seedExpired(ctx context.Context, seed *heldSeed) {
	if ctx.Err() != nil {
		return
	}
	h.seeds.mu.Lock()
	held := h.seeds.byPane[seed.pane] == seed
	if held {
		delete(h.seeds.byPane, seed.pane)
	}
	h.seeds.mu.Unlock()
	if !held {
		return
	}
	log.Printf("spawn: %s never registered on pane %s within %s; opening message dropped", seed.name, seed.pane, spawnSeedHold)
	h.ask.reg.AddEvent(ctx, "spawn_seed_failed", map[string]any{"pane_id": seed.pane, "display_name": seed.name, "from_peer": seed.from, "error": "peer did not register before timeout"})
	h.tellSpawner(ctx, seed, fmt.Sprintf("[spawn] %s never registered on pane %s within %s; its opening message was dropped. Send it again once the peer is on the mesh.",
		seed.name, seed.pane, spawnSeedHold))
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
