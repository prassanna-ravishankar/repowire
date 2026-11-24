package relayserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/hub"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

type meshDaemon struct {
	local *httptest.Server
	hub   *hub.Hub
}

// startMeshDaemon runs a full daemon (registry, tracker, delivery, hub)
// joined to the relay the way main.go joins it.
func startMeshDaemon(t *testing.T, ctx context.Context, f *authFixture, id string) *meshDaemon {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	transport := service.NewWebSocketTransport()
	registry, err := peer.NewRegistry(ctx, store, noProcesses{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.NewHubWithTransport(registry, transport, "")
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(registry, h.Router(), transport, asks, nil)
	t.Cleanup(delivery.Close)
	h.WithReadDeps(asks, store).WithAskLifecycle(asks, delivery, registry).WithMessaging(delivery, store)
	mux := http.NewServeMux()
	h.Routes(mux)
	local := httptest.NewServer(mux)
	t.Cleanup(local.Close)
	client := relay.NewClient("ws"+strings.TrimPrefix(f.http.URL, "http"), f.key, id, local.URL)
	if err := h.WithRemote(ctx, client, id); err != nil {
		t.Fatal(err)
	}
	client.WithRemoteHandler(h.RemoteHandler())
	client.Start(ctx)
	t.Cleanup(client.Stop)
	waitForDaemon(t, f.http.URL, f.key, id)
	return &meshDaemon{local: local, hub: h}
}

func (d *meshDaemon) agent(t *testing.T, ctx context.Context, path string) (proto.PeerID, chan map[string]any) {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(d.local.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := wsjson.Write(ctx, conn, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: proto.DisplayName(filepath.Base(path)), Circle: "proj", Backend: proto.AgentClaudeCode, Role: proto.RoleAgent, Path: &path}); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, conn, &connected); err != nil || connected.Type != proto.FrameConnected {
		t.Fatalf("connect %v %+v", err, connected)
	}
	frames := make(chan map[string]any, 8)
	go func() {
		for {
			var frame map[string]any
			if wsjson.Read(ctx, conn, &frame) != nil {
				return
			}
			if id, ok := frame["delivery_id"].(string); ok && id != "" {
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "delivery_ack", "delivery_id": id, "status": "delivered"})
			}
			if frame["type"] != "ping" {
				frames <- frame
			}
		}
	}()
	return connected.SessionID, frames
}

// An ask tunneled through the relay into one daemon reaches a peer on
// another daemon, through the same relay, without deadlocking the tunnel.
func TestTunneledAskCrossesMachines(t *testing.T) {
	f := newAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	alpha := startMeshDaemon(t, ctx, f, "alpha")
	beta := startMeshDaemon(t, ctx, f, "beta")
	askerID, askerFrames := alpha.agent(t, ctx, "/work/asker")
	bobID, bobFrames := beta.agent(t, ctx, "/work/bob")

	owner, _ := f.s.tokens.validate(f.key)
	body, _ := json.Marshal(map[string]string{"from_peer": string(askerID), "to_peer": string(bobID) + "@beta", "text": "tunneled"})
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ask", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	f.s.tunnel(w, r, f.s.daemon(owner.UserID, "alpha"), "/ask")
	var sent map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &sent)
	if w.Code != 200 || sent["error"] != nil {
		t.Fatalf("tunneled ask %d %s", w.Code, w.Body.String())
	}
	cid := sent["correlation_id"].(string)
	select {
	case frame := <-bobFrames:
		if frame["correlation_id"] != cid {
			t.Fatalf("bob got %v", frame)
		}
	case <-ctx.Done():
		t.Fatal("bob never got the ask")
	}

	ack, _ := json.Marshal(map[string]string{"correlation_id": cid, "message": "over the relay", "from_peer": string(bobID)})
	res, err := http.Post(beta.local.URL+"/ack", "application/json", strings.NewReader(string(ack)))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("ack %v %v", res, err)
	}
	res.Body.Close()
	select {
	case frame := <-askerFrames:
		if !strings.Contains(frame["text"].(string), "] over the relay") {
			t.Fatalf("asker got %v", frame)
		}
	case <-ctx.Done():
		t.Fatal("reply never reached the asker")
	}
}
