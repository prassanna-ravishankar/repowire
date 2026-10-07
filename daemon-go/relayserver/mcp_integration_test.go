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

type noProcesses struct{}

func (noProcesses) PIDAlive(int) bool { return false }

// Uses real HTTP, both WebSocket hops, the production registry/router/tracker,
// and an agent fixture that receives and acknowledges work. No live mesh hooks.
func TestRelayMCPAgentRoundTrip(t *testing.T) {
	f := newAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	transport := service.NewWebSocketTransport()
	registry, err := peer.NewRegistry(ctx, store, noProcesses{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	daemonHub := hub.NewHubWithTransport(registry, transport, "local-secret")
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(registry, daemonHub.Router(), transport, asks, nil)
	defer delivery.Close()
	daemonHub.WithAskLifecycle(asks, delivery, registry).WithMessaging(delivery, store).WithReviews(hub.NewReviewQueueStore(filepath.Join(t.TempDir(), "reviews.json")))
	mux := http.NewServeMux()
	daemonHub.Routes(mux)
	local := httptest.NewServer(mux)
	defer local.Close()
	connector := relay.NewClient("ws"+strings.TrimPrefix(f.http.URL, "http"), f.key, "real-machine", local.URL).WithAuthToken("local-secret")
	connector.Start(ctx)
	defer connector.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for !connector.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !connector.Status().Connected {
		t.Fatal("relay did not connect")
	}
	agent, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(local.URL, "http")+"/ws?token=local-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.CloseNow()
	authToken := "local-secret"
	if err := wsjson.Write(ctx, agent, proto.ConnectFrame{AuthToken: &authToken, Type: proto.FrameConnect, DisplayName: "round-trip-worker", Circle: "test", Backend: proto.AgentClaudeCode, Role: proto.RoleAgent}); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, agent, &connected); err != nil {
		t.Fatal(err)
	}
	if connected.Type != proto.FrameConnected {
		t.Fatalf("connect rejected: %#v", connected)
	}
	deadline = time.Now().Add(time.Second)
	for !transport.IsConnected(connected.SessionID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	tokens := f.tokens(t, oauthRead+" "+oauthWrite)
	access := tokens["access_token"].(string)
	call := func(name string, args any) map[string]any {
		t.Helper()
		status, result := f.rpc(t, access, "tools/call", map[string]any{"name": name, "arguments": args})
		if status != 200 || result["error"] != nil {
			t.Fatalf("%s HTTP %d: %v", name, status, result)
		}
		out := result["result"].(map[string]any)
		if out["isError"] == true {
			t.Fatalf("%s: %v", name, out)
		}
		if data, ok := out["structuredContent"].(map[string]any); ok {
			return data
		}
		var data map[string]any
		content := out["content"].([]any)
		_ = json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &data)
		return data
	}
	listing := call("list_agents", map[string]any{"daemon_id": "real-machine"})
	if len(listing["agents"].([]any)) != 1 {
		t.Fatalf("agent discovery %v", listing)
	}
	received := make(chan string, 1)
	go func() {
		for {
			var frame map[string]any
			if wsjson.Read(ctx, agent, &frame) != nil {
				return
			}
			if frame["type"] == "ping" {
				_ = wsjson.Write(ctx, agent, map[string]any{"type": "pong"})
				continue
			}
			if cid, ok := frame["correlation_id"].(string); ok && cid != "" {
				received <- cid
				return
			}
		}
	}()
	sent := call("ask_agent", map[string]any{"daemon_id": "real-machine", "peer_id": string(connected.SessionID), "message": "Reply with the marker relay-round-trip"})
	requestID := sent["request_id"].(string)
	select {
	case cid := <-received:
		if cid != requestID {
			t.Fatal("wrong correlation ID")
		}
	case <-ctx.Done():
		t.Fatal("agent did not receive ask")
	}
	pending := call("list_requests", map[string]any{"daemon_id": "real-machine"})
	if len(pending["requests"].([]any)) != 1 {
		t.Fatalf("pending %v", pending)
	}
	// Fast reply arrives before any get_reply call.
	req, _ := http.NewRequestWithContext(ctx, "POST", local.URL+"/ack", strings.NewReader(`{"correlation_id":"`+requestID+`","message":"relay-round-trip"}`))
	req.Header.Set("Authorization", "Bearer local-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("agent ack %d", res.StatusCode)
	}
	reply := call("get_reply", map[string]any{"daemon_id": "real-machine", "request_id": requestID})
	if reply["status"] != "resolved" || reply["reply"] != "relay-round-trip" {
		t.Fatalf("reply %v", reply)
	}
	// Notification delivery uses the same concrete target and reports acceptance.
	notification := make(chan map[string]any, 1)
	go func() {
		var frame map[string]any
		if wsjson.Read(ctx, agent, &frame) == nil {
			notification <- frame
		}
	}()
	sentMessage := call("send_message", map[string]any{"daemon_id": "real-machine", "peer_id": string(connected.SessionID), "message": "follow-up-marker"})
	if sentMessage["delivered"] != true {
		t.Fatalf("notify acceptance: %v", sentMessage)
	}
	select {
	case frame := <-notification:
		if frame["text"] != "follow-up-marker" {
			t.Fatalf("notification: %v", frame)
		}
	case <-ctx.Done():
		t.Fatal("notification did not reach agent")
	}

	// New grants cannot read another app connection's request.
	other := f.tokens(t, oauthRead)
	_, result := f.rpc(t, other["access_token"].(string), "tools/call", map[string]any{"name": "get_reply", "arguments": map[string]any{"daemon_id": "real-machine", "request_id": requestID}})
	if result["result"].(map[string]any)["isError"] != true {
		t.Fatal("cross-grant reply leaked")
	}
	// Machine selection cannot be spoofed with another namespace's daemon ID.
	otherKey, _ := f.s.tokens.validate("rw_another-owner-secret")
	f.s.registerConn(&daemonConn{userID: otherKey.UserID, daemonID: "other-machine"})
	_, result = f.rpc(t, access, "tools/call", map[string]any{"name": "list_agents", "arguments": map[string]any{"daemon_id": "other-machine"}})
	if result["result"].(map[string]any)["isError"] != true {
		t.Fatal("cross-owner machine exposed")
	}
	connector.Stop()
	_, result = f.rpc(t, access, "tools/call", map[string]any{"name": "list_agents", "arguments": map[string]any{"daemon_id": "real-machine"}})
	if result["result"].(map[string]any)["isError"] != true {
		t.Fatal("offline machine did not fail loudly")
	}
}
