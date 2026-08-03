package codexbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

func TestDeliveryUsesNativeThreadState(t *testing.T) {
	for _, test := range []struct{ name, activeTurn, want string }{
		{"active", "turn-1", "turn/steer"},
		{"idle", "", "turn/start"},
	} {
		t.Run(test.name, func(t *testing.T) { testDeliveryMethod(t, test.activeTurn, test.want) })
	}
}

func testDeliveryMethod(t *testing.T, activeTurn, want string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := make(chan string, 1)
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var request map[string]any
		if wsjson.Read(ctx, conn, &request) == nil {
			method <- stringValue(request, "method")
			_ = wsjson.Write(ctx, conn, map[string]any{"id": request["id"], "result": map[string]any{"turnId": "turn-1"}})
		}
	}))
	defer appServer.Close()
	appConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	go b.readApp()

	meshConn := make(chan *websocket.Conn, 1)
	meshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			meshConn <- conn
		}
	}))
	defer meshServer.Close()
	meshClient, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(meshServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-meshConn
	defer serverConn.CloseNow()

	p := &threadPeer{bridge: b, id: "thread-1", activeTurn: activeTurn}
	p.inject(ctx, meshClient, map[string]any{"type": "ask", "delivery_id": "delivery-1", "text": "review this"})
	select {
	case got := <-method:
		if got != want {
			t.Fatalf("method = %q, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("App Server received no delivery")
	}
	var ack map[string]any
	if err := wsjson.Read(ctx, serverConn, &ack); err != nil {
		t.Fatal(err)
	}
	if stringValue(ack, "status") != "accepted" || stringValue(ack, "delivery_id") != "delivery-1" {
		t.Fatalf("ack = %#v", ack)
	}
}

func TestThreadHelpers(t *testing.T) {
	thread := map[string]any{"turns": []any{map[string]any{"id": "done", "status": "completed"}, map[string]any{"id": "live", "status": "inProgress"}}}
	if got := activeTurn(thread); got != "live" {
		t.Fatalf("activeTurn = %q", got)
	}
	if got := safeName("a project/name"); got != "a-project-name" {
		t.Fatalf("safeName = %q", got)
	}
}

func TestTmuxPlacementRequiresOneMatchingCodexPane(t *testing.T) {
	out := "/work/repo\tmesh\t@7\t100\n/work/other\tother\t@8\t200\n"
	processes := "100 1 zsh\n101 100 node /usr/local/bin/codex\n200 1 zsh\n"
	if circle, source := parseTmuxPlacement(out, processes, "/work/repo", proto.CircleBoundarySession); circle != "mesh" || source != "tmux" {
		t.Fatalf("placement = %q %q", circle, source)
	}
	out += "/work/repo\tother-mesh\t@9\t300\n"
	processes += "300 1 /opt/codex\n"
	if circle, _ := parseTmuxPlacement(out, processes, "/work/repo", proto.CircleBoundarySession); circle != "" {
		t.Fatalf("ambiguous placement should fail loud, got %q", circle)
	}
}
