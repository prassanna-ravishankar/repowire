package relay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestTunnelDeadlineFollowsTheRelay(t *testing.T) {
	for name, tc := range map[string]struct {
		msg  map[string]any
		want time.Duration
	}{
		"none (an older relay)": {map[string]any{}, tunnelTimeout},
		"stated":                {map[string]any{"timeout_ms": float64(55000)}, 55 * time.Second},
		"sub-second":            {map[string]any{"timeout_ms": float64(250)}, 250 * time.Millisecond},
		"over the cap":          {map[string]any{"timeout_ms": float64(600000)}, maxTunnelTimeout},
		"huge":                  {map[string]any{"timeout_ms": float64(1e30)}, maxTunnelTimeout},
		"zero":                  {map[string]any{"timeout_ms": float64(0)}, tunnelTimeout},
		"negative":              {map[string]any{"timeout_ms": float64(-1)}, tunnelTimeout},
		"not a number":          {map[string]any{"timeout_ms": "55000"}, tunnelTimeout},
	} {
		if got := tunnelDeadline(tc.msg); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// The relay's timeout_ms bounds the local request both ways (#437): a route
// that waits past the old 30s default is answered when the relay allows it,
// and a request is cut at the deadline the relay stated, not later.
func TestTunneledRequestUsesTheRelaysDeadline(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(400 * time.Millisecond): // a long-poll route
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	})
	results := make(chan int, 2)
	relay, _ := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		for i, ms := range []float64{2000, 100} {
			_ = wsjson.Write(ctx, c, map[string]any{
				"type": "http_request", "request_id": []string{"long", "short"}[i], "method": "POST", "path": "/asks/a/wait", "timeout_ms": ms,
			})
			var resp map[string]any
			if wsjson.Read(ctx, c, &resp) != nil {
				return
			}
			results <- int(resp["status"].(float64))
		}
	})
	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()
	for _, want := range []int{http.StatusOK, http.StatusBadGateway} {
		select {
		case got := <-results:
			if got != want {
				t.Fatalf("status %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no http_response")
		}
	}
}
