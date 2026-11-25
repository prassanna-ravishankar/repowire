package mobile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// holdDaemon is a daemon socket that answers connect, sends one surface_hold
// for releaseAt, and reports when (and whether) surface_release arrives.
func holdDaemon(t *testing.T, releaseAt func() time.Time) (*httptest.Server, <-chan time.Time) {
	t.Helper()
	released := make(chan time.Time, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		var connect map[string]any
		if wsjson.Read(ctx, conn, &connect) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "connected", "session_id": "repow-tg", "display_name": "telegram"})
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "surface_hold", "held": 2, "release_at": releaseAt().UTC().Format(time.RFC3339)})
		for {
			var msg map[string]any
			if wsjson.Read(ctx, conn, &msg) != nil {
				return
			}
			if msg["type"] == "surface_release" {
				released <- time.Now()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, released
}

func runPeer(t *testing.T, server *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peer := NewDaemonPeer(server.URL, "", "telegram", "/telegram", "mobile")
	go func() { _ = peer.Run(ctx, func(context.Context, map[string]any) error { return nil }) }()
}

// The bridge holds the timer: it sends surface_release at release_at, not
// before.
func TestBridgeReleasesAtTheNamedInstant(t *testing.T) {
	var at time.Time
	server, released := holdDaemon(t, func() time.Time {
		// RFC 3339 carries whole seconds: name the next one plus one.
		at = time.Now().Truncate(time.Second).Add(2 * time.Second)
		return at
	})
	runPeer(t, server)
	select {
	case got := <-released:
		if got.Before(at) {
			t.Fatalf("released %v before release_at %v", got, at)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no surface_release")
	}
}

// A release_at already past (a daemon that was down at the window's end, a
// late reconnect) releases at once.
func TestBridgeReleasesAtOnceWhenTheInstantHasPassed(t *testing.T) {
	server, released := holdDaemon(t, func() time.Time { return time.Now().Add(-time.Hour) })
	runPeer(t, server)
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("a past release_at did not release at once")
	}
}
