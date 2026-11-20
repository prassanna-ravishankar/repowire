package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

// ackPeer is one live socket for id on tr: frames it receives arrive on
// frames, and when ack is set it answers each with a delivery_ack.
type ackPeer struct {
	frames chan map[string]any
	client *websocket.Conn
	server *websocket.Conn
}

func connectAckPeer(t *testing.T, tr *WebSocketTransport, id proto.PeerID, ack bool) *ackPeer {
	return connectAckPeerReady(t, tr, id, ack, false)
}

// connectAckPeerReady connects like the hub does when awaitReady is set: the
// socket takes frames only after tr.Ready.
func connectAckPeerReady(t *testing.T, tr *WebSocketTransport, id proto.PeerID, ack, awaitReady bool) *ackPeer {
	t.Helper()
	p := &ackPeer{frames: make(chan map[string]any, 4)}
	connected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		p.server = c
		tr.Connect(context.Background(), &ConnectionInfo{SessionID: id, WS: c, AwaitReady: awaitReady})
		close(connected)
		for {
			var data map[string]any
			if wsjson.Read(context.Background(), c, &data) != nil {
				return
			}
			if data["type"] == "delivery_ack" {
				tr.ResolveDeliveryAck(id, data["delivery_id"].(string), data)
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseNow() })
	p.client = client
	<-connected
	go func() {
		for {
			var frame map[string]any
			if wsjson.Read(context.Background(), client, &frame) != nil {
				return
			}
			p.frames <- frame
			if ack {
				_ = wsjson.Write(context.Background(), client, map[string]any{"type": "delivery_ack", "delivery_id": frame["delivery_id"], "status": "accepted"})
			}
		}
	}()
	return p
}

func (p *ackPeer) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case f := <-p.frames:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
		return nil
	}
}

type ackResult struct {
	receipt map[string]any
	err     error
}

func sendAwaitingAck(tr *WebSocketTransport, id proto.PeerID, timeout time.Duration) chan ackResult {
	out := make(chan ackResult, 1)
	go func() {
		receipt, err := tr.SendAndWaitDeliveryAck(context.Background(), id, map[string]any{"delivery_id": "d-1", "type": "notify", "text": "reply"}, timeout)
		out <- ackResult{receipt, err}
	}()
	return out
}

// TestAckWaitFailsWhenTheSocketCloses (#472): a frame written to a socket that
// closes before its delivery_ack, with no connection to replace it, is not
// delivered. It used to wait out the timeout and return (nil, nil), which
// Notify reported as delivered.
func TestAckWaitFailsWhenTheSocketCloses(t *testing.T) {
	tr := NewWebSocketTransport()
	id := proto.PeerID("peer-a")
	old := connectAckPeer(t, tr, id, false)
	result := sendAwaitingAck(tr, id, 2*time.Second)
	old.next(t) // the hook has the frame on the wire but never accepts it
	_ = tr.Close(id)
	select {
	case r := <-result:
		if r.receipt != nil || !errors.Is(r.err, ErrNotConnected) {
			t.Fatalf("closed before ack = %v, %v; want ErrNotConnected", r.receipt, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("the wait outlived its closed socket")
	}
}

// TestAckWaitFollowsAReplacedSocket: when a newer connection replaces the one
// the frame went out on, the frame is sent again there, under the same
// delivery_id and the same overall timeout, and its ack is the receipt.
func TestAckWaitFollowsAReplacedSocket(t *testing.T) {
	tr := NewWebSocketTransport()
	id := proto.PeerID("peer-a")
	old := connectAckPeer(t, tr, id, false)
	result := sendAwaitingAck(tr, id, 2*time.Second)
	old.next(t)
	fresh := connectAckPeer(t, tr, id, true)
	if f := fresh.next(t); f["delivery_id"] != "d-1" {
		t.Fatalf("replacement got %v", f)
	}
	r := <-result
	if r.err != nil || r.receipt == nil || r.receipt["status"] != "accepted" {
		t.Fatalf("replaced socket = %v, %v; want the replacement's receipt", r.receipt, r.err)
	}
}

// TestAckWaitTimeoutOnAnOpenSocketIsStillBestEffort: an older hook that never
// acks keeps the (nil, nil) answer while its socket stays open.
func TestAckWaitTimeoutOnAnOpenSocketIsStillBestEffort(t *testing.T) {
	tr := NewWebSocketTransport()
	id := proto.PeerID("peer-a")
	connectAckPeer(t, tr, id, false)
	r := <-sendAwaitingAck(tr, id, 150*time.Millisecond)
	if r.err != nil || r.receipt != nil {
		t.Fatalf("timeout on an open socket = %v, %v; want nil, nil", r.receipt, r.err)
	}
}

// Review #562: a frame re-sent to a replacement waits for the replacement's
// handshake (Ready), so it never arrives ahead of the connected frame.
func TestAckWaitResendWaitsForTheReplacementHandshake(t *testing.T) {
	tr := NewWebSocketTransport()
	id := proto.PeerID("peer-a")
	old := connectAckPeer(t, tr, id, false)
	result := sendAwaitingAck(tr, id, 3*time.Second)
	old.next(t)
	fresh := connectAckPeerReady(t, tr, id, true, true)
	select {
	case f := <-fresh.frames:
		t.Fatalf("frame %v reached the replacement before its handshake", f)
	case <-time.After(150 * time.Millisecond):
	}
	tr.Ready(id, fresh.server)
	if f := fresh.next(t); f["delivery_id"] != "d-1" {
		t.Fatalf("replacement got %v", f)
	}
	if r := <-result; r.err != nil || r.receipt == nil {
		t.Fatalf("after the handshake = %v, %v; want the replacement's receipt", r.receipt, r.err)
	}
}
