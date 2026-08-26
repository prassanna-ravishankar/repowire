package hooks

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

func TestClaudeInboxDelivery(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "claude.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []map[string]any, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		decoder := json.NewDecoder(conn)
		var auth, message map[string]any
		if decoder.Decode(&auth) == nil && decoder.Decode(&message) == nil {
			received <- []map[string]any{auth, message}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	meshConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			meshConn <- conn
		}
	}))
	defer server.Close()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	peer := <-meshConn
	defer peer.CloseNow()

	t.Setenv("REPOWIRE_BACKEND", "claude-code")
	t.Setenv(claudeMessagingSocketEnv, "uds:"+socket)
	t.Setenv(claudeMessagingTokenEnv, "session-token")
	stop, strikes := handleMessage(ctx, client, map[string]any{
		"type": "ask", "delivery_id": "delivery-1", "from_peer": "reviewer", "to_peer": "owner",
		"correlation_id": "ask-1", "text": "review this",
	}, "", "", proto.CircleBoundarySession, 0)
	if stop || strikes != 0 {
		t.Fatalf("native delivery stopped hook: stop=%v strikes=%d", stop, strikes)
	}

	frames := <-received
	if frames[0]["type"] != "auth" || frames[0]["token"] != "session-token" {
		t.Fatalf("Claude inbox auth = %#v", frames[0])
	}
	message := frames[1]
	prompt, _ := message["message"].(map[string]any)
	content, _ := prompt["content"].(string)
	if message["type"] != "user" || prompt["role"] != "user" || !strings.Contains(content, `correlation-id="ask-1"`) {
		t.Fatalf("Claude inbox message = %#v", message)
	}
	var ack map[string]any
	if err := wsjson.Read(ctx, peer, &ack); err != nil {
		t.Fatal(err)
	}
	if ack["status"] != "accepted" || ack["delivery_id"] != "delivery-1" {
		t.Fatalf("delivery ack = %#v", ack)
	}
}

func TestRuntimeInboxCapabilityRequiresClaudeSocket(t *testing.T) {
	t.Setenv(claudeMessagingSocketEnv, "")
	if got := transportCapabilities("claude-code"); len(got) != 1 || got[0] != proto.CapDeliveryReceipts {
		t.Fatalf("capabilities without socket = %v", got)
	}
	t.Setenv(claudeMessagingSocketEnv, "/tmp/claude.sock")
	if got := transportCapabilities("claude-code"); len(got) != 2 || got[1] != proto.CapRuntimeInbox {
		t.Fatalf("Claude capabilities = %v", got)
	}
	if got := transportCapabilities("gemini"); len(got) != 1 {
		t.Fatalf("Gemini capabilities = %v", got)
	}
}
