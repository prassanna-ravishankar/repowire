package hub

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// brokenFlushWriter writes into a recorder but fails every flush, as a
// dashboard connection reset mid-stream does.
type brokenFlushWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *brokenFlushWriter) Flush() { _ = w.FlushError() }
func (w *brokenFlushWriter) FlushError() error {
	w.cancel()
	return errors.New("connection reset by peer")
}

// TestDashboardFailedFlushIsNotReceipted (review of #478): only a batch that
// reached the dashboard counts as its push receipt.
func TestDashboardFailedFlushIsNotReceipted(t *testing.T) {
	h := newTestHub(t)
	h.dashboard = newDashboardSurface(func() {})
	h.reg.AddEvent(context.Background(), "inbox_item", map[string]any{"item_id": "inb-broken"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &brokenFlushWriter{httptest.NewRecorder(), cancel}
	h.handleEventsStream(w, httptest.NewRequest("GET", "/events/stream?surface=dashboard", nil).WithContext(ctx))

	pushCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := h.dashboard.Push(pushCtx, &state.InboxItem{ID: "inb-broken"}); err == nil {
		t.Fatal("a failed flush produced a dashboard receipt")
	}
	if h.dashboard.Live() {
		t.Fatal("a stream that ended still counts as a live dashboard")
	}
}

// TestDashboardFlushedItemIsReceipted is the success side: a push that starts
// after its event was flushed still finds it.
func TestDashboardFlushedItemIsReceipted(t *testing.T) {
	d := newDashboardSurface(func() {})
	d.wrote([]map[string]any{{"type": "inbox_item", "item_id": "inb-1"}, {"type": "peer_online"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Push(ctx, &state.InboxItem{ID: "inb-1"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Push(ctx, &state.InboxItem{ID: "inb-2"}) }()
	time.Sleep(10 * time.Millisecond)
	d.wrote([]map[string]any{{"type": "inbox_item", "item_id": "inb-2"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
