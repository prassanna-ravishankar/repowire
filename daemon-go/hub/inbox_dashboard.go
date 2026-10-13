package hub

import (
	"context"
	"sync"

	"github.com/repowire/repowire/daemon-go/state"
)

// dashboardSurface renders the human inbox on the dashboard (#466). The
// dashboard has no peer and no socket of its own: its channel is the event
// stream it already holds open (GET /events/stream?surface=dashboard), which
// the relay tunnels too. It is live while at least one such stream is open.
// A push is received when an open stream has flushed the item's inbox_item
// event; the browser then reads the item through GET /inbox.
type dashboardSurface struct {
	mu      sync.Mutex
	streams int
	// flushed holds the most recent item ids a stream has written, so a push
	// that starts after the stream already flushed its event still succeeds.
	flushed []string
	waiters map[string][]chan struct{}
	// onPresence republishes surface presence when a stream opens or closes.
	onPresence func()
}

// dashboardFlushedCap bounds the remembered flushes; it only has to outlast
// the gap between an item's event and the start of its push.
const dashboardFlushedCap = 256

func newDashboardSurface(onPresence func()) *dashboardSurface {
	return &dashboardSurface{waiters: map[string][]chan struct{}{}, onPresence: onPresence}
}

func (d *dashboardSurface) Name() string { return "dashboard" }

func (d *dashboardSurface) Live() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streams > 0
}

// Push waits until an open dashboard stream has flushed the item's event.
func (d *dashboardSurface) Push(ctx context.Context, item *state.InboxItem) error {
	d.mu.Lock()
	for _, id := range d.flushed {
		if id == item.ID {
			d.mu.Unlock()
			return nil
		}
	}
	ch := make(chan struct{})
	d.waiters[item.ID] = append(d.waiters[item.ID], ch)
	d.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		waiters := d.waiters[item.ID]
		for n, w := range waiters {
			if w == ch {
				d.waiters[item.ID] = append(waiters[:n], waiters[n+1:]...)
				break
			}
		}
		if len(d.waiters[item.ID]) == 0 {
			delete(d.waiters, item.ID)
		}
		d.mu.Unlock()
		return ctx.Err()
	}
}

// attach registers an open dashboard stream; the returned func detaches it.
func (d *dashboardSurface) attach() func() {
	d.mu.Lock()
	d.streams++
	d.mu.Unlock()
	d.onPresence()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			d.streams--
			d.mu.Unlock()
			d.onPresence()
		})
	}
}

// wrote records the inbox items among events a stream has just flushed.
func (d *dashboardSurface) wrote(events []map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ev := range events {
		if ev["type"] != "inbox_item" {
			continue
		}
		id, _ := ev["item_id"].(string)
		if id == "" {
			continue
		}
		d.flushed = append(d.flushed, id)
		for _, ch := range d.waiters[id] {
			close(ch)
		}
		delete(d.waiters, id)
	}
	if over := len(d.flushed) - dashboardFlushedCap; over > 0 {
		d.flushed = d.flushed[over:]
	}
}
