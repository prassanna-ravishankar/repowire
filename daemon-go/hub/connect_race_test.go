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

// The connect registers its peer as attaching, so the lazy repair it starts
// cannot demote the peer before the socket is attached, and ends that window
// once it is (#569).
func TestConnectRegistersAttachingUntilTheSocketIsLive(t *testing.T) {
	h := newTestHub(t)
	var attachingBefore bool
	h.beforeConnect = func(id proto.PeerID) { attachingBefore = h.reg.Attaching(id) }
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: "bob", Circle: "c", Backend: proto.AgentClaudeCode, Role: proto.RoleAgent}); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, conn, &connected); err != nil || connected.Type != proto.FrameConnected {
		t.Fatalf("connect: %v %+v", err, connected)
	}
	if !attachingBefore || h.reg.Attaching(connected.SessionID) {
		t.Fatalf("attaching before socket = %v, after = %v; want true, false", attachingBefore, h.reg.Attaching(connected.SessionID))
	}
	if p, ok := h.reg.GetPeer(connected.SessionID); !ok || p.Status != proto.StatusOnline {
		t.Fatalf("peer after connect = %+v, want online", p)
	}
}
