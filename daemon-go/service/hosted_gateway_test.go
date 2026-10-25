package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The gateway acknowledges settled reports and reconnects after the runtime
// drops the socket.
func TestHostedGatewayAcksReportsAndReconnects(t *testing.T) {
	conns := make(chan *websocket.Conn)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway" || r.Header.Get("Authorization") != "Bearer secret" {
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
	hosted.Connect(srv.URL+"/", "secret")

	report := func(conn *websocket.Conn, id string) gatewayFrame {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wsjson.Write(ctx, conn, gatewayFrame{Type: "settled", ID: id, Agent: "repow-x", OperationID: "op-1", Status: "done", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
		var ack gatewayFrame
		if err := wsjson.Read(ctx, conn, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	// A settled report is acknowledged by id once handled, known or not.
	first := <-conns
	if ack := report(first, "outbox:repow-x:op-1"); ack.Type != "ack" || ack.ID != "outbox:repow-x:op-1" {
		t.Fatalf("ack = %+v", ack)
	}
	first.Close(websocket.StatusGoingAway, "redeploy")

	select {
	case second := <-conns:
		if ack := report(second, "outbox:repow-x:op-1"); ack.Type != "ack" {
			t.Fatalf("resent report not acknowledged: %+v", ack)
		}
		second.Close(websocket.StatusNormalClosure, "")
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not reconnect")
	}
}
