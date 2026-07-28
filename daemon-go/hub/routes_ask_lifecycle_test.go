package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// askFakeRegistry satisfies BOTH accessRegistry (for PeerDelivery) and
// askRoutesRegistry (for the route handlers). It resolves by display_name or
// peer_id out of an in-memory slice; unknown targets error.
type askFakeRegistry struct {
	*fakeRegistry
	byPane map[string]*proto.Peer
}

func newAskFakeRegistry(peers ...*proto.Peer) *askFakeRegistry {
	return &askFakeRegistry{
		fakeRegistry: &fakeRegistry{peers: peers},
		byPane:       map[string]*proto.Peer{},
	}
}

func (r *askFakeRegistry) GetPeerByPane(pane string) (*proto.Peer, bool) {
	p, ok := r.byPane[pane]
	return p, ok
}

func (r *askFakeRegistry) GetPeerByName(name string, circle *string) (*proto.Peer, error) {
	if p := r.lookup(name); p != nil {
		return p, nil
	}
	return nil, nil
}

// newAskTestHub builds a hub with the ask-lifecycle deps wired over fakes, plus
// the httptest server serving its mux. The transport's ackFrame drives the WS
// delivery result.
func newAskTestHub(t *testing.T, reg *askFakeRegistry, f *fakeTransport) (*httptest.Server, *service.AskTracker) {
	return newAskTestHubWithQueue(t, reg, f, nil)
}

func newAskTestHubWithQueue(t *testing.T, reg *askFakeRegistry, f *fakeTransport, queue *fakeQueue) (*httptest.Server, *service.AskTracker) {
	t.Helper()
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(reg, newRouterWithFake(f), f, asks, queue)
	h := &Hub{authToken: ""}
	h.WithAskLifecycle(asks, delivery, reg)

	mux := http.NewServeMux()
	h.registerAskLifecycleRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, asks
}

func TestPostAckQueuedReplyClosesWithoutDuplicate(t *testing.T) {
	asker := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	responder := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(asker, responder)
	queue := &fakeQueue{}
	srv, asks := newAskTestHubWithQueue(t, reg, &fakeTransport{ackErr: service.ErrNotConnected}, queue)
	cid, err := asks.Register(context.Background(), service.RegisterAskParams{
		FromPeerID: asker.PeerID, FromPeerName: asker.DisplayName,
		ToPeerID: responder.PeerID, ToPeerName: responder.DisplayName, Text: "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	reply := "durable reply"
	resp := postJSON(t, srv.URL+"/ack", AckRequest{CorrelationID: cid, Message: &reply})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queued ack status = %d, want 200", resp.StatusCode)
	}
	closed, _ := asks.Get(cid)
	if !closed.Closed || closed.CloseReason != "ack_with_msg" || closed.ReplyText == nil || *closed.ReplyText != reply {
		t.Fatalf("ask not closed with captured reply: %+v", closed)
	}
	if len(queue.enqueued) != 1 {
		t.Fatalf("queued rows = %d, want 1", len(queue.enqueued))
	}

	// A retrying client can safely bare-ack the now-closed ask without another row.
	retry := postJSON(t, srv.URL+"/ack", AckRequest{CorrelationID: cid})
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK || len(queue.enqueued) != 1 {
		t.Fatalf("retry status=%d queued rows=%d", retry.StatusCode, len(queue.enqueued))
	}
}

// postJSON is the shared test helper (routes_events_test.go).

// TestPostAskRegistersAndDelivers is the primary endpoint test: POST /ask with a
// live WS recipient registers an open ask, delivers it over the fake transport,
// and returns 200 with a minted ask-<hex8> correlation_id. The ask is then
// surfaced as inbound on GET /asks/pending for the recipient.
func TestPostAskRegistersAndDelivers(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	from := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(from, target)
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}, ackDelay: 0}
	srv, asks := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ask", AskRequest{
		FromPeer: "alpha", ToPeer: "beta", Text: "need a hand?",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out AskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.CorrelationID) < 4 || out.CorrelationID[:4] != "ask-" {
		t.Fatalf("expected minted ask-<hex8> cid, got %q", out.CorrelationID)
	}
	if f.lastTarget != target.PeerID {
		t.Fatalf("ask must route by PeerID, got %s", f.lastTarget)
	}

	// The open ask is the Stop-hook reminder source for the recipient (inbound).
	pending, err := asks.PendingForPeer(context.Background(), target.PeerID, -1, "inbound")
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].CorrelationID != out.CorrelationID {
		t.Fatalf("expected the open ask surfaced inbound, got %+v", pending)
	}
}

// TestPostAskInjectionFailureIs503 is the fail-loud contract: a delivery_ack with
// status=rejected (hook reached, pane rejected) yields 503 {injection_failed},
// the ask is closed send_failed, and the live peer is NOT marked offline.
func TestPostAskInjectionFailureIs503(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(target)
	f := &fakeTransport{ackFrame: map[string]any{"status": "rejected", "detail": "pane not safe"}, ackDelay: 0}
	srv, asks := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ask", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "do it"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on injection failure, got %d", resp.StatusCode)
	}
	if len(reg.offlined) != 0 {
		t.Fatalf("injection failure on a live socket must NOT mark the peer offline, got %v", reg.offlined)
	}
	if asks.OpenCount() != 0 {
		t.Fatalf("an injection-failed ask must be closed send_failed, open=%d", asks.OpenCount())
	}
}

// TestPostAckBareClosesIdempotently: a bare ack closes the open ask and returns
// 200; a second bare ack of the now-closed ask is idempotent (still 200).
func TestPostAckBareClosesIdempotently(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(target)
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}, ackDelay: 0}
	srv, asks := newAskTestHub(t, reg, f)

	cid, err := asks.Register(context.Background(), service.RegisterAskParams{
		FromPeerID: "repow-default-aaaa", FromPeerName: "alpha",
		ToPeerID: target.PeerID, ToPeerName: target.DisplayName, Text: "q",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	resp := postJSON(t, srv.URL+"/ack", AckRequest{CorrelationID: cid})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bare ack expected 200, got %d", resp.StatusCode)
	}
	if asks.OpenCount() != 0 {
		t.Fatalf("bare ack must close the ask, open=%d", asks.OpenCount())
	}
	// Idempotent re-ack of a closed ask.
	resp2 := postJSON(t, srv.URL+"/ack", AckRequest{CorrelationID: cid})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("idempotent re-ack expected 200, got %d", resp2.StatusCode)
	}
}

// TestPostAckUnknownIs404: acking an unknown correlation_id is a 404.
func TestPostAckUnknownIs404(t *testing.T) {
	reg := newAskFakeRegistry()
	f := &fakeTransport{}
	srv, _ := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ack", AckRequest{CorrelationID: "ask-nope"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown ask, got %d", resp.StatusCode)
	}
}

// TestPendingAsksRequiresExactlyOneID: /asks/pending with neither or both of
// pane_id/peer_id is a 400.
func TestPendingAsksRequiresExactlyOneID(t *testing.T) {
	reg := newAskFakeRegistry()
	f := &fakeTransport{}
	srv, _ := newAskTestHub(t, reg, f)

	for _, q := range []string{"", "?pane_id=p&peer_id=x"} {
		resp, err := http.Get(srv.URL + "/asks/pending" + q)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q: expected 400, got %d", q, resp.StatusCode)
		}
	}
}

// TestAskWaitRejectsNonAsker: /asks/{cid}/wait is 403 when peer_id is not the
// original asker (waiting flips the ask to pull delivery — only the asker may).
func TestAskWaitRejectsNonAsker(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(target)
	f := &fakeTransport{}
	srv, asks := newAskTestHub(t, reg, f)

	cid, _ := asks.Register(context.Background(), service.RegisterAskParams{
		FromPeerID: "repow-default-aaaa", FromPeerName: "alpha",
		ToPeerID: target.PeerID, ToPeerName: target.DisplayName, Text: "q",
	})

	resp := postJSON(t, srv.URL+"/asks/"+cid+"/wait", AskWaitRequest{PeerID: "intruder"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-asker wait expected 403, got %d", resp.StatusCode)
	}
}

func TestAskManyFansOutAndAggregates(t *testing.T) {
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	gamma := peerWith("repow-default-cccc", "gamma", "default", proto.StatusOnline)
	reg := newAskFakeRegistry(alpha, beta, gamma)
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}}
	srv, _ := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ask-many", AskManyRequest{
		FromPeer: "alpha",
		ToPeers:  []string{"beta", "missing", "gamma", "beta"},
		Text:     "status?",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ask-many expected 200, got %d", resp.StatusCode)
	}
	var opened AskManyResponse
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if len(opened.Children) != 3 {
		t.Fatalf("expected beta, missing, gamma children with beta deduped, got %+v", opened.Children)
	}
	if opened.Children[0].CorrelationID == nil {
		t.Fatalf("beta child should have a cid: %+v", opened.Children[0])
	}
	if opened.Children[1].Error == nil {
		t.Fatalf("missing child should carry an error: %+v", opened.Children[1])
	}

	reply := "done"
	ack := postJSON(t, srv.URL+"/ack", AckRequest{
		CorrelationID: *opened.Children[0].CorrelationID,
		Message:       &reply,
	})
	ack.Body.Close()
	if ack.StatusCode != http.StatusOK {
		t.Fatalf("ack child expected 200, got %d", ack.StatusCode)
	}

	res, err := http.Get(srv.URL + "/ask-many/" + opened.ParentID)
	if err != nil {
		t.Fatalf("GET aggregate: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("aggregate expected 200, got %d", res.StatusCode)
	}
	var agg map[string]any
	if err := json.NewDecoder(res.Body).Decode(&agg); err != nil {
		t.Fatalf("decode aggregate: %v", err)
	}
	if agg["state"] != "pending" {
		t.Fatalf("state = %v, want pending", agg["state"])
	}
	rollup := agg["rollup"].(map[string]any)
	if rollup["total"].(float64) != 3 || rollup["replied"].(float64) != 1 ||
		rollup["pending"].(float64) != 1 || rollup["failed"].(float64) != 1 {
		t.Fatalf("unexpected rollup: %#v", rollup)
	}
}

func TestAskManyRejectsEmptyPeerList(t *testing.T) {
	reg := newAskFakeRegistry()
	f := &fakeTransport{}
	srv, _ := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ask-many", AskManyRequest{FromPeer: "alpha", Text: "q"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty ask-many should be 422, got %d", resp.StatusCode)
	}
}
