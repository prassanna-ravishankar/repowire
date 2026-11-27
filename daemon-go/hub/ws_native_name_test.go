package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

// A WebSocket connect carrying the runtime's session name answers with that
// name as the address (OpenCode and Pi register this way inside tmux).
func TestWSConnectAppliesNativeName(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := wsjson.Write(ctx, c, map[string]any{
		"type": "connect", "display_name": "app", "circle": "c", "circle_source": "tmux",
		"backend": "opencode", "path": "/work/app", "agent_pid": 4242, "native_name": "Fix Telegram access",
	}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := wsjson.Read(ctx, c, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["display_name"] != "fix-telegram-access" {
		t.Fatalf("connected = %v", reply)
	}
	if p, _ := h.reg.GetPeer(proto.PeerID(reply["session_id"].(string))); p == nil || p.DisplayName != "fix-telegram-access" {
		t.Fatalf("registry = %+v", p)
	}
}
