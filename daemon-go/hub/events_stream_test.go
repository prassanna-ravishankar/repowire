package hub

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newStreamRig builds a hub with a real registry and an httptest server serving
// ONLY the SSE stream route, so the test is independent of the other HTTP
// groups' wiring. Auth is disabled (empty token via newTestHub).
func newStreamRig(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.EventsStreamRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

// readDataFrame reads SSE lines until it sees a `data: ` frame, returning its
// payload, or fails on timeout/EOF. Keepalive comment frames (`: ...`) and blank
// separators are skipped.
func readDataFrame(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning SSE stream: %v", err)
	}
	t.Fatal("stream closed before a data frame arrived")
	return ""
}

// TestEventsStreamFlushesThenStreamsLive is the primary path: a pre-existing
// event is delivered in the initial flush, then an event pushed live after the
// connection is open arrives as a second data frame. This proves subscribe +
// initial flush + wake-on-AddEvent all wire together.
func TestEventsStreamFlushesThenStreamsLive(t *testing.T) {
	h, srv := newStreamRig(t)

	// Buffer one event before connecting — it must appear in the initial flush.
	h.reg.AddEvent(context.Background(), "chat_turn", map[string]any{"peer": "a", "text": "first"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events/stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if xb := resp.Header.Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}

	sc := bufio.NewScanner(resp.Body)

	// Initial flush: the pre-existing event.
	if got := readDataFrame(t, sc); !strings.Contains(got, `"first"`) {
		t.Fatalf("initial flush frame = %q, want it to contain %q", got, "first")
	}

	// Push a live event; the handler must wake and stream it. Small delay so the
	// handler has entered its select loop (the initial-flush drain is done).
	time.Sleep(50 * time.Millisecond)
	h.reg.AddEvent(context.Background(), "chat_turn", map[string]any{"peer": "b", "text": "second"})

	if got := readDataFrame(t, sc); !strings.Contains(got, `"second"`) {
		t.Fatalf("live frame = %q, want it to contain %q", got, "second")
	}
}

// TestEventsStreamRejectsNonGet asserts the handler fails loud on a wrong method
// rather than holding a connection open.
func TestEventsStreamRejectsNonGet(t *testing.T) {
	_, srv := newStreamRig(t)
	resp, err := http.Post(srv.URL+"/events/stream", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /events/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
