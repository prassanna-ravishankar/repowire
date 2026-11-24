package service

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

type echoTools struct{}

func (echoTools) RunHostedTool(_ context.Context, peerID proto.PeerID, tool string, args json.RawMessage) (string, bool) {
	return string(peerID) + " " + tool + " " + string(args), tool == "refused"
}

// The gateway carries a call from the runtime to the tool runner and its
// result back, acknowledges settled reports, and reconnects after the runtime
// drops the socket.
func TestHostedGatewayRunsCallsAndReconnects(t *testing.T) {
	conns := make(chan *websocket.Conn)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway" || r.Header.Get("Authorization") != "Bearer secret" ||
			r.Header.Get("X-Repowire-Machine") != "test" || r.Header.Get("X-Repowire-Version") != "9.9.9" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conns <- conn
	}))
	defer srv.Close()

	hosted := NewHosted(&fakeHostedRegistry{}, newFakeHostedRuntime(), nil, "test")
	defer hosted.Close()
	hosted.Connect(srv.URL+"/", "secret", "9.9.9", echoTools{})

	call := func(conn *websocket.Conn, tool string) gatewayFrame {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wsjson.Write(ctx, conn, gatewayFrame{Type: "call", ID: "c-" + tool, Agent: "repow-x", Tool: tool, Args: json.RawMessage(`{"a":1}`)}); err != nil {
			t.Fatal(err)
		}
		var result gatewayFrame
		if err := wsjson.Read(ctx, conn, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := <-conns
	if got := call(first, "whoami"); got.Type != "result" || got.ID != "c-whoami" || got.Text != `repow-x whoami {"a":1}` || got.Error {
		t.Fatalf("result = %+v", got)
	}
	// A settled report is acknowledged by id once handled, known or not.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, first, gatewayFrame{Type: "settled", ID: "outbox:repow-x:op-1", Agent: "repow-x", OperationID: "op-1", Status: "done", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	var ack gatewayFrame
	if err := wsjson.Read(ctx, first, &ack); err != nil || ack.Type != "ack" || ack.ID != "outbox:repow-x:op-1" {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	first.Close(websocket.StatusGoingAway, "redeploy")

	select {
	case second := <-conns:
		if got := call(second, "refused"); !got.Error {
			t.Fatalf("refused call not flagged: %+v", got)
		}
		second.Close(websocket.StatusNormalClosure, "")
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not reconnect")
	}
}

// The daemon names its retention policy on every gateway connect, in
// seconds, so the owner's Gateway sweeps by the same policy as the daemon.
func TestHostedGatewayNamesTheRetentionPolicy(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("X-Repowire-Retention"):
		default:
		}
		http.Error(w, "enough", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	hosted := NewHosted(&fakeHostedRegistry{}, newFakeHostedRuntime(), nil, "test").
		WithRetention(state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 0, AskLife: 24 * time.Hour})
	defer hosted.Close()
	hosted.Connect(srv.URL, "secret", "9.9.9", echoTools{})
	select {
	case header := <-got:
		if header != `{"ask_life_s":86400,"content_s":604800,"records_s":0}` {
			t.Fatalf("X-Repowire-Retention = %q", header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never dialled")
	}
}
