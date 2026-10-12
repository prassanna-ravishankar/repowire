package hub

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// askHistory counts every ask ever addressed to each peer, and per
// (sender, target) pair, for the routing snapshot. The in-memory AskTracker
// forgets after 24h, so the counts come from the durable stores (events
// journal and delivery-trace ledger) and are read once, on first use, then
// kept current by the tracker's register callback. Lookups are map reads.
// historyLoadTimeout bounds the one-time read of the durable ask history.
const historyLoadTimeout = 5 * time.Second

type askHistory struct {
	load func(context.Context) ([]state.AskRef, error)

	mu     sync.Mutex
	loaded bool
	seen   map[string]struct{}
	to     map[string]int
	pair   map[[2]string]int
}

func newAskHistory(load func(context.Context) ([]state.AskRef, error)) *askHistory {
	return &askHistory{load: load, seen: map[string]struct{}{}, to: map[string]int{}, pair: map[[2]string]int{}}
}

// add counts an ask once per correlation id. Caller holds mu.
func (a *askHistory) add(cid, from, to string) {
	if _, dup := a.seen[cid]; dup {
		return
	}
	a.seen[cid] = struct{}{}
	a.to[to]++
	a.pair[[2]string{from, to}]++
}

// note counts a newly registered ask. Safe before the first load: the load
// deduplicates by correlation id.
func (a *askHistory) note(cid string, from, to proto.PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.add(cid, string(from), string(to))
}

// ready loads the durable history once. It reports false, and stays unloaded
// so the next call retries, when the stores cannot be read; the snapshot then
// omits the fields rather than reporting counts it does not have.
func (a *askHistory) ready(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return true
	}
	if a.load == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, historyLoadTimeout)
	defer cancel()
	refs, err := a.load(ctx)
	if err != nil {
		log.Printf("routing: prior-ask history not loaded: %v", err)
		return false
	}
	for _, r := range refs {
		a.add(r.CorrelationID, r.From, r.To)
	}
	a.loaded = true
	return true
}

// counts returns the asks ever addressed to peer, and how many of them sender
// sent. ok is false when the history is unavailable.
func (a *askHistory) counts(ctx context.Context, sender, peer proto.PeerID) (total, fromSender int, ok bool) {
	if a == nil || !a.ready(ctx) {
		return 0, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.to[string(peer)], a.pair[[2]string{string(sender), string(peer)}], true
}
