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

// The lazy repair a connect starts can demote the new peer in the window
// between its registration and the attach of its socket. The handler undoes
// that, so a peer holding a live socket is never left offline.
func TestConnectSurvivesADemotionBeforeItsSocketAttaches(t *testing.T) {
	h := newTestHub(t)
	h.beforeConnect = func(id proto.PeerID) {
		_, _ = h.reg.MarkOffline(context.Background(), id, false)
	}
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
	if p, ok := h.reg.GetPeer(connected.SessionID); !ok || p.Status != proto.StatusOnline {
		t.Fatalf("peer after connect = %+v, want online", p)
	}
}
