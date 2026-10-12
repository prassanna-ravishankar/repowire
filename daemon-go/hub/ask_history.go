package hub

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// askHistory counts every ask ever sent and ever addressed to each peer, and
// per (sender, target) pair, for the routing snapshot. The in-memory
// AskTracker forgets after 24h, so the counts come from the durable stores
// (events journal and delivery-trace ledger). They are read once in the
// background when the hub starts and kept current by the tracker's register
// callback. A decision never waits for the read: until it finishes, counts
// report unavailable and the snapshot omits the fields.
type askHistory struct {
	load func(context.Context) ([]state.AskRef, error)

	loaded atomic.Bool
	done   chan struct{} // closed once the history is loaded

	mu   sync.Mutex
	seen map[string]struct{}
	to   map[string]int
	sent map[string]int
	pair map[[2]string]int
}

// Background load: each attempt is bounded, and a failure is retried with a
// capped backoff, off every request path.
const (
	historyLoadTimeout     = 30 * time.Second
	historyLoadAttempts    = 12
	historyLoadBackoffCap  = time.Minute
	historyLoadBackoffInit = time.Second
)

func newAskHistory(load func(context.Context) ([]state.AskRef, error)) *askHistory {
	return &askHistory{
		load: load, done: make(chan struct{}),
		seen: map[string]struct{}{}, to: map[string]int{}, sent: map[string]int{}, pair: map[[2]string]int{},
	}
}

// add counts an ask once per correlation id. Caller holds mu.
func (a *askHistory) add(cid, from, to string) {
	if _, dup := a.seen[cid]; dup {
		return
	}
	a.seen[cid] = struct{}{}
	a.to[to]++
	a.sent[from]++
	a.pair[[2]string{from, to}]++
}

// note counts a newly registered ask. Safe before and during the load: the
// load deduplicates by correlation id.
func (a *askHistory) note(cid string, from, to proto.PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.add(cid, string(from), string(to))
}

// start reads the durable history in the background.
func (a *askHistory) start() { go a.run() }

func (a *askHistory) run() {
	if a.load == nil {
		return
	}
	backoff := historyLoadBackoffInit
	for attempt := 1; attempt <= historyLoadAttempts; attempt++ {
		began := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), historyLoadTimeout)
		refs, err := a.load(ctx)
		cancel()
		if err == nil {
			a.mu.Lock()
			for _, r := range refs {
				a.add(r.CorrelationID, r.From, r.To)
			}
			a.mu.Unlock()
			a.loaded.Store(true)
			close(a.done)
			log.Printf("routing: prior-ask history loaded: %d asks in %s", len(refs), time.Since(began).Round(time.Millisecond))
			return
		}
		log.Printf("routing: prior-ask history not loaded (attempt %d of %d): %v", attempt, historyLoadAttempts, err)
		time.Sleep(backoff)
		backoff = min(backoff*2, historyLoadBackoffCap)
	}
	log.Printf("routing: prior-ask history unavailable; route_decision rows omit prior-ask counts until the daemon restarts")
}

// counts returns the asks ever addressed to peer, the asks it ever sent, and
// how many of the addressed ones sender sent. ok is false until the history
// has loaded; it never blocks.
func (a *askHistory) counts(sender, peer proto.PeerID) (total, sent, fromSender int, ok bool) {
	if a == nil || !a.loaded.Load() {
		return 0, 0, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.to[string(peer)], a.sent[string(peer)], a.pair[[2]string{string(sender), string(peer)}], true
}
