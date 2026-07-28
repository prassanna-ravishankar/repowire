package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// TestHealthWireShape verifies GET /health, registered via h.Routes on a real
// mux, returns the exact JSON the dashboard/clients depend on: status "ok",
// a numeric live-peer count, and the SQLite schema_version. With no peers
// connected the count is zero.
func TestHealthWireShape(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["status"] != "ok" {
		t.Fatalf("status = %v, want \"ok\"", body["status"])
	}
	// JSON numbers decode to float64.
	if got, ok := body["peers"].(float64); !ok || got != 0 {
		t.Fatalf("peers = %v, want 0", body["peers"])
	}
	if got, ok := body["schema_version"].(float64); !ok || int(got) != state.SchemaVersion {
		t.Fatalf("schema_version = %v, want %d", body["schema_version"], state.SchemaVersion)
	}
}

// TestHealthCountsLivePeers confirms the peer count is the live transport
// session count: after one peer connects over /ws, /health reports peers:1.
func TestHealthCountsLivePeers(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):] + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer c.CloseNow()

	if err := wsjson.Write(ctx, c, proto.ConnectFrame{
		Type:        proto.FrameConnect,
		DisplayName: proto.DisplayName("alpha"),
		Circle:      "default",
		Backend:     proto.AgentClaudeCode,
		Role:        proto.RoleAgent,
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, c, &connected); err != nil {
		t.Fatalf("read connected: %v", err)
	}

	// Wait for the transport to register the live socket.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.transport.IsConnected(connected.SessionID) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, ok := body["peers"].(float64); !ok || got != 1 {
		t.Fatalf("peers = %v, want 1", body["peers"])
	}
}
