package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// noopRepairer satisfies lazyRepairer; LazyRepairAsync is a maintenance
// side-effect the routes kick off and never await, so the test only needs it
// to not panic.
type noopRepairer struct{}

func (noopRepairer) LazyRepairAsync(context.Context) {}

// captureTracer satisfies deliveryTracer and records the stage sequence so the
// test can assert the truthful-trace breadcrumbs (created/resolved_peer/...).
type captureTracer struct{ stages []string }

func (c *captureTracer) RecordTrace(_ context.Context, _, _, stage, _, _, _, _ string, _ map[string]any) error {
	c.stages = append(c.stages, stage)
	return nil
}

// newMessagingTestRig builds a MessagingRoutes over a real PeerDelivery wired to
// the shared fakes (fakeRegistry/fakeTransport/fakeQueue in hub_test.go),
// plus an httptest server. auth is disabled (empty token) via an identity
// wrapper so handler behavior is what is under test.
func newMessagingTestRig(t *testing.T, reg *fakeRegistry, ft *fakeTransport) (*httptest.Server, *captureTracer) {
	t.Helper()
	router := newRouterWithFake(ft)
	delivery := service.NewPeerDelivery(reg, router, ft, nil, &fakeQueue{})
	tr := &captureTracer{}
	mr := NewMessagingRoutes(delivery, noopRepairer{}, tr)

	mux := http.NewServeMux()
	identityAuth := func(next http.HandlerFunc) http.HandlerFunc { return next }
	mr.Register(mux, identityAuth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tr
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestNotifyHandlerTransportDelivered is the happy WS path through the HTTP
// surface: a connected target whose hook acks "injected" returns
// sent/delivered/transport_delivered with the resolved identity and a minted
// notif-delivery id, and the trace records created→resolved_peer→pane_injected.
func TestNotifyHandlerTransportDelivered(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{
		peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline),
		target,
	}}
	ft := &fakeTransport{
		connected: map[proto.PeerID]bool{target.PeerID: true},
		ackFrame:  map[string]any{"status": "injected", "delivery_id": "notif-x"},
	}

	srv, tr := newMessagingTestRig(t, reg, ft)
	resp := postJSON(t, srv.URL+"/notify", map[string]any{
		"from_peer": "alpha",
		"to_peer":   "beta",
		"text":      "ping",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body notifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.OK || body.Status != "sent" || body.DeliveryState != "delivered" {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	if body.Reason != "transport_delivered" {
		t.Errorf("reason = %q, want transport_delivered", body.Reason)
	}
	if !body.Delivered || body.Queued {
		t.Errorf("delivered/queued = %v/%v, want true/false", body.Delivered, body.Queued)
	}
	if body.ToPeerID == nil || *body.ToPeerID != "repow-default-bbbb" {
		t.Errorf("to_peer_id = %v, want repow-default-bbbb", body.ToPeerID)
	}
	if body.ToPeerName == nil || *body.ToPeerName != "beta" {
		t.Errorf("to_peer_name = %v, want beta", body.ToPeerName)
	}
	if body.DeliveryID == nil || len(*body.DeliveryID) == 0 {
		t.Errorf("delivery_id missing: %v", body.DeliveryID)
	}
	// Truthful trace: pane_injected only because the hook acked injected.
	want := []string{"created", "resolved_peer", "hook_received", "pane_injected"}
	if !equalStrings(tr.stages, want) {
		t.Errorf("trace stages = %v, want %v", tr.stages, want)
	}
}

// TestNotifyHandlerUnknownPeer maps a CheckAccess "Unknown peer" error to 404
// with the fail-loud JSON body (ok=false, reason=unknown_peer), not a bare 500.
func TestNotifyHandlerUnknownPeer(t *testing.T) {
	reg := &fakeRegistry{checkErr: errors.New("Unknown peer: ghost")}
	ft := &fakeTransport{connected: map[proto.PeerID]bool{}}
	srv, _ := newMessagingTestRig(t, reg, ft)

	resp := postJSON(t, srv.URL+"/notify", map[string]any{
		"from_peer": "alpha", "to_peer": "ghost", "text": "hi",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != false || body["reason"] != "unknown_peer" {
		t.Errorf("body = %v, want ok=false reason=unknown_peer", body)
	}
}

// TestNotifyHandlerNoLiveTransport maps a no-live-transport send (queue disabled
// → fail loud) to 503 with a no_connection trace stage.
func TestNotifyHandlerNoLiveTransport(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	ft := &fakeTransport{
		connected: map[proto.PeerID]bool{}, // not connected → service.ErrNotConnected on ack
		ackErr:    service.ErrNotConnected,
	}
	router := newRouterWithFake(ft)
	// store=nil → queued-delivery disabled → no-live-transport fails loud.
	delivery := service.NewPeerDelivery(reg, router, ft, nil, nil)
	tr := &captureTracer{}
	mr := NewMessagingRoutes(delivery, noopRepairer{}, tr)
	mux := http.NewServeMux()
	mr.Register(mux, func(n http.HandlerFunc) http.HandlerFunc { return n })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/notify", map[string]any{
		"from_peer": "alpha", "to_peer": "beta", "text": "hi",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !containsString(tr.stages, "no_connection") {
		t.Errorf("trace stages = %v, want a no_connection stage", tr.stages)
	}
}

// TestBroadcastHandler fans out to connected peers and returns the delivered
// display-names in the Python wire shape (sent_to / failed).
func TestBroadcastHandler(t *testing.T) {
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	gamma := peerWith("repow-default-cccc", "gamma", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{alpha, beta, gamma}}
	ft := &fakeTransport{
		connected: map[proto.PeerID]bool{beta.PeerID: true, gamma.PeerID: true},
		sessions:  []proto.PeerID{beta.PeerID, gamma.PeerID},
	}
	srv, _ := newMessagingTestRig(t, reg, ft)

	resp := postJSON(t, srv.URL+"/broadcast", map[string]any{
		"from_peer": "alpha", "text": "all hands", "exclude": []string{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body broadcastResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Errorf("ok = false, want true")
	}
	if len(body.SentTo) != 2 {
		t.Errorf("sent_to = %v, want 2 recipients (beta, gamma)", body.SentTo)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
