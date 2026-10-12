package hub

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// askHistory counts every ask ever sent and ever addressed to each peer, and
// per (sender, target) pair, for the routing snapshot. The in-memory
// AskTracker forgets after 24h, so the counts come from the durable stores
// (events journal and delivery-trace ledger). A background loader reads them
// once after the hub starts; the tracker's register callback keeps them
// current. A decision never waits for the load: until it finishes, counts
// report unavailable and the snapshot omits the fields.
//
// Locking: the loader builds the whole index off the lock, then publishes it
// under mu, where the only work is folding in the asks registered during the
// load window and swapping the pointer. That critical section is bounded by
// the number of asks registered during the load, sub-millisecond in practice
// (3,000 pending asks take about 0.7 ms against a load of about 250 ms).
// Everywhere else mu guards a lookup or a single increment, so the tracker's
// Register (which calls note while holding its own lock) is never held up by
// the merge itself.
type askHistory struct {
	load func(context.Context) ([]state.AskRef, error)
	// beforeMerge, set by tests, runs off the lock while the index is built.
	beforeMerge func()

	mu      sync.Mutex
	idx     *askIndex // nil until the load has finished
	pending []askRef  // asks registered while the load ran
	done    chan struct{}

	cancel context.CancelFunc
	exited chan struct{}
}

type askRef struct{ cid, from, to string }

// askIndex is the counted history.
type askIndex struct {
	seen map[string]struct{}
	to   map[string]int
	sent map[string]int
	pair map[[2]string]int
}

func newAskIndex() *askIndex {
	return &askIndex{seen: map[string]struct{}{}, to: map[string]int{}, sent: map[string]int{}, pair: map[[2]string]int{}}
}

// add counts an ask once per correlation id.
func (x *askIndex) add(cid, from, to string) {
	if _, dup := x.seen[cid]; dup {
		return
	}
	x.seen[cid] = struct{}{}
	x.to[to]++
	x.sent[from]++
	x.pair[[2]string{from, to}]++
}

// Background load: each read is bounded, and a failure is retried with a
// capped backoff, off every request path and cancellable at every wait.
const (
	historyLoadTimeout     = 30 * time.Second
	historyLoadAttempts    = 12
	historyLoadBackoffCap  = time.Minute
	historyLoadBackoffInit = time.Second
)

func newAskHistory(load func(context.Context) ([]state.AskRef, error)) *askHistory {
	return &askHistory{load: load, done: make(chan struct{}), exited: make(chan struct{})}
}

// note counts a newly registered ask. Called under the tracker's lock, so it
// only appends or increments under mu.
func (a *askHistory) note(cid string, from, to proto.PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.idx != nil {
		a.idx.add(cid, string(from), string(to))
		return
	}
	a.pending = append(a.pending, askRef{cid, string(from), string(to)})
}

// start launches the loader under ctx (the daemon's lifetime).
func (a *askHistory) start(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	go func() {
		defer close(a.exited)
		a.run(ctx)
	}()
}

// stop cancels the loader and waits for it at most bound, so shutdown never
// waits on a stuck read.
func (a *askHistory) stop(bound time.Duration) {
	if a == nil || a.cancel == nil {
		return
	}
	a.cancel()
	select {
	case <-a.exited:
	case <-time.After(bound):
		log.Printf("routing: prior-ask loader did not stop within %s", bound)
	}
}

func (a *askHistory) run(ctx context.Context) {
	if a.load == nil {
		return
	}
	backoff := historyLoadBackoffInit
	for attempt := 1; attempt <= historyLoadAttempts; attempt++ {
		began := time.Now()
		readCtx, cancel := context.WithTimeout(ctx, historyLoadTimeout)
		refs, err := a.load(readCtx)
		cancel()
		if err == nil {
			a.publish(refs)
			log.Printf("routing: prior-ask history loaded: %d asks in %s", len(refs), time.Since(began).Round(time.Millisecond))
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("routing: prior-ask history not loaded (attempt %d of %d): %v", attempt, historyLoadAttempts, err)
		if attempt == historyLoadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, historyLoadBackoffCap)
	}
	log.Printf("routing: prior-ask history unavailable; route_decision rows omit prior-ask counts until the daemon restarts")
}

// publish builds the index off the lock, then, under mu, folds in the asks
// registered meanwhile and swaps it in.
func (a *askHistory) publish(refs []state.AskRef) {
	if a.beforeMerge != nil {
		a.beforeMerge()
	}
	idx := newAskIndex()
	for _, r := range refs {
		idx.add(r.CorrelationID, r.From, r.To)
	}
	a.mu.Lock()
	for _, r := range a.pending {
		idx.add(r.cid, r.from, r.to)
	}
	a.pending = nil
	a.idx = idx
	a.mu.Unlock()
	close(a.done)
}

// counts returns the asks ever addressed to peer, the asks it ever sent, and
// how many of the addressed ones sender sent. ok is false until the history
// has loaded. It takes mu only for the lookup.
func (a *askHistory) counts(sender, peer proto.PeerID) (total, sent, fromSender int, ok bool) {
	if a == nil {
		return 0, 0, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.idx == nil {
		return 0, 0, 0, false
	}
	return a.idx.to[string(peer)], a.idx.sent[string(peer)], a.idx.pair[[2]string{string(sender), string(peer)}], true
}
