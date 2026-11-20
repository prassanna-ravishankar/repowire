package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// recordingTransport keeps every frame per peer, so a test can count what a
// peer received, and refuses peers marked offline as a dead socket would.
type recordingTransport struct {
	mu      sync.Mutex
	frames  map[proto.PeerID][]map[string]any
	offline map[proto.PeerID]bool
	// slow holds each send this long, widening race windows in tests.
	slow time.Duration
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{frames: map[proto.PeerID][]map[string]any{}, offline: map[proto.PeerID]bool{}}
}

func (r *recordingTransport) record(id proto.PeerID, v any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offline[id] {
		return service.ErrNotConnected
	}
	frame, _ := v.(map[string]any)
	r.frames[id] = append(r.frames[id], frame)
	return nil
}

func (r *recordingTransport) Send(_ context.Context, id proto.PeerID, v any) error {
	return r.record(id, v)
}

func (r *recordingTransport) SendAndWaitDeliveryAck(_ context.Context, id proto.PeerID, v any, _ time.Duration) (map[string]any, error) {
	r.mu.Lock()
	slow := r.slow
	r.mu.Unlock()
	time.Sleep(slow)
	if err := r.record(id, v); err != nil {
		return nil, err
	}
	return map[string]any{"status": "accepted"}, nil
}

func (r *recordingTransport) IsConnected(id proto.PeerID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.offline[id]
}
func (r *recordingTransport) GetAllSessions() []proto.PeerID               { return nil }
func (r *recordingTransport) ConnectionPaneID(proto.PeerID) (string, bool) { return "", false }
func (r *recordingTransport) Ping(context.Context, proto.PeerID, time.Duration) (map[string]any, error) {
	return nil, nil
}
func (r *recordingTransport) ACPRoute(*proto.Peer) (*service.ACPRouteDecision, bool) {
	return nil, false
}

func (r *recordingTransport) received(id proto.PeerID) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames[id])
}

// stageTracer records trace stages; safe for concurrent requests.
type stageTracer struct {
	mu     sync.Mutex
	stages []string
}

func (s *stageTracer) RecordTrace(_ context.Context, _, _, stage, _, _, _, _ string, _ map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stages = append(s.stages, stage)
	return nil
}

func (s *stageTracer) has(stage string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.stages, stage)
}

type delegateRig struct {
	srv      *httptest.Server
	asks     *service.AskTracker
	wire     *recordingTransport
	messages *service.Messages
	store    *state.Store
	traces   *stageTracer
}

var (
	alphaPeer = func() *proto.Peer { return peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline) }
	betaPeer  = func() *proto.Peer { return peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline) }
	gammaPeer = func() *proto.Peer { return peerWith("repow-default-cccc", "gamma", "default", proto.StatusOnline) }
	deltaPeer = func() *proto.Peer { return peerWith("repow-default-dddd", "delta", "default", proto.StatusOnline) }
)

func newDelegateRig(t *testing.T, peers ...*proto.Peer) *delegateRig {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := newAskFakeRegistry(peers...)
	wire := newRecordingTransport()
	asks := service.NewAskTracker(0)
	messages := service.NewMessages(store)
	delivery := service.NewPeerDelivery(reg, service.NewMessageRouter(wire, nil), wire, asks, nil).WithMessages(messages)
	traces := &stageTracer{}
	h := &Hub{deliveryTraces: traces, store: store}
	h.WithAskLifecycle(asks, delivery, reg)
	mux := http.NewServeMux()
	h.registerAskLifecycleRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &delegateRig{srv: srv, asks: asks, wire: wire, messages: messages, store: store, traces: traces}
}

// post sends body to path and decodes the JSON reply into a map.
func (r *delegateRig) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	resp := postJSON(t, r.srv.URL+path, body)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *delegateRig) ask(t *testing.T, from, to *proto.Peer, text string) string {
	t.Helper()
	status, out := r.post(t, "/ask", AskRequest{FromPeer: string(from.PeerID), ToPeer: string(to.DisplayName), Text: text})
	cid, _ := out["correlation_id"].(string)
	msgID, _ := out["message_id"].(string)
	if status != http.StatusOK || cid == "" || !strings.HasPrefix(msgID, "msg-") {
		t.Fatalf("ask: %d %v", status, out)
	}
	return cid
}

func (r *delegateRig) delegate(t *testing.T, cid string, by, to *proto.Peer, note string) (int, map[string]any) {
	t.Helper()
	from := string(by.PeerID)
	return r.post(t, "/asks/"+cid+"/delegate", DelegateRequest{ToPeer: string(to.DisplayName), Note: note, FromPeer: &from})
}

func (r *delegateRig) ack(t *testing.T, cid string, by *proto.Peer, message string) (int, map[string]any) {
	t.Helper()
	from := string(by.PeerID)
	return r.post(t, "/ack", AckRequest{CorrelationID: cid, Message: &message, FromPeer: &from})
}

func (r *delegateRig) pending(t *testing.T, p *proto.Peer, direction string) []PendingAsk {
	t.Helper()
	resp, err := http.Get(r.srv.URL + "/asks/pending?peer_id=" + string(p.PeerID) + "&direction=" + direction)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out PendingAsksResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Asks
}

func frameTexts(frames []map[string]any) []string {
	var out []string
	for _, f := range frames {
		text, _ := f["text"].(string)
		out = append(out, text)
	}
	return out
}

func TestDelegatedAskReplyReachesAskerExactlyOnce(t *testing.T) {
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "review the parser?")

	status, out := r.delegate(t, cid, beta, gamma, "owns the parser")
	if status != http.StatusOK || out["delegated_to"] != "gamma" || out["correlation_id"] != cid {
		t.Fatalf("delegate: %d %v", status, out)
	}
	if hops, _ := out["delegations"].([]any); len(hops) != 1 {
		t.Fatalf("delegations = %v", out["delegations"])
	}
	got := r.wire.received(gamma.PeerID)
	if len(got) != 1 || got[0]["type"] != "ask" || got[0]["correlation_id"] != cid || got[0]["from_peer"] != "alpha" ||
		!strings.HasPrefix(got[0]["text"].(string), "[delegated by @beta: owns the parser]\nreview the parser?") {
		t.Fatalf("gamma received %v", got)
	}
	if !r.traces.has("delegated") {
		t.Fatal("no delegated trace stage")
	}
	msg, _ := r.messages.Get(context.Background(), got[0]["message_id"].(string))
	if msg == nil || msg.Kind != state.MessageAsk || msg.CorrelationID != cid || msg.PeerID != string(gamma.PeerID) {
		t.Fatalf("gamma's message record = %+v", msg)
	}
	if mine := r.pending(t, beta, "inbound"); len(mine) != 0 {
		t.Fatalf("beta is still reminded: %+v", mine)
	}
	if theirs := r.pending(t, gamma, "inbound"); len(theirs) != 1 || len(theirs[0].Delegations) != 1 || theirs[0].Delegations[0].From != "beta" {
		t.Fatalf("gamma's pending = %+v", theirs)
	}

	if status, out := r.ack(t, cid, beta, "I'll take it back"); status != http.StatusConflict || out["detail"].(map[string]any)["error"] != "ask_delegated" {
		t.Fatalf("former holder ack: %d %v", status, out)
	}
	if status, out := r.ack(t, cid, gamma, "done"); status != http.StatusOK {
		t.Fatalf("holder ack: %d %v", status, out)
	}
	if status, _ := r.ack(t, cid, gamma, "done again"); status != http.StatusGone {
		t.Fatalf("second ack: %d", status)
	}
	if status, _ := r.post(t, "/decline", DeclineRequest{CorrelationID: cid, Reason: "late", FromPeer: strp(string(beta.PeerID))}); status == http.StatusOK {
		t.Fatal("former holder declined a closed ask")
	}
	replies := frameTexts(r.wire.received(alpha.PeerID))
	if want := []string{"[ack #" + cid + " from @gamma via @beta] done"}; !slices.Equal(replies, want) {
		t.Fatalf("alpha received %q, want %q", replies, want)
	}
}

func TestDelegationChainNamesEveryHop(t *testing.T) {
	alpha, beta, gamma, delta := alphaPeer(), betaPeer(), gammaPeer(), deltaPeer()
	r := newDelegateRig(t, alpha, beta, gamma, delta)
	cid := r.ask(t, alpha, beta, "who owns the release?")
	if status, out := r.delegate(t, cid, beta, gamma, ""); status != http.StatusOK {
		t.Fatalf("first hop: %d %v", status, out)
	}
	if status, out := r.delegate(t, cid, gamma, delta, "release manager"); status != http.StatusOK {
		t.Fatalf("second hop: %d %v", status, out)
	}
	if text := frameTexts(r.wire.received(delta.PeerID)); len(text) != 1 || !strings.HasPrefix(text[0], "[delegated by @gamma: release manager]\n") {
		t.Fatalf("delta received %q", text)
	}
	if mine := r.pending(t, alpha, "outbound"); len(mine) != 1 || mine[0].ToPeer != "delta" || len(mine[0].Delegations) != 2 {
		t.Fatalf("asker's view = %+v", mine)
	}
	if status, _ := r.ack(t, cid, delta, "me"); status != http.StatusOK {
		t.Fatalf("ack: %d", status)
	}
	if replies := frameTexts(r.wire.received(alpha.PeerID)); !slices.Equal(replies, []string{"[ack #" + cid + " from @delta via @beta, @gamma] me"}) {
		t.Fatalf("alpha received %q", replies)
	}
}

func TestDelegateToOfflinePeerLeavesAskWithDelegator(t *testing.T) {
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "review?")
	r.wire.mu.Lock()
	r.wire.offline[gamma.PeerID] = true
	r.wire.mu.Unlock()

	status, out := r.delegate(t, cid, beta, gamma, "")
	if status != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(out["detail"]), "stays with you") {
		t.Fatalf("delegate to offline peer: %d %v", status, out)
	}
	ask, _ := r.asks.Get(cid)
	if ask.Closed || ask.ToPeerID != beta.PeerID || len(ask.Delegations) != 0 {
		t.Fatalf("ask after a failed hand-off = %+v", ask)
	}
	if !r.traces.has("delegate_failed") {
		t.Fatal("no delegate_failed trace stage")
	}
	if msgs, _ := r.messages.List(context.Background(), state.PeerMessageFilter{PeerID: string(gamma.PeerID)}); len(msgs) != 0 {
		t.Fatalf("gamma has a record of an ask it never got: %+v", msgs)
	}
	if mine := r.pending(t, beta, "inbound"); len(mine) != 1 {
		t.Fatalf("beta lost the reminder: %+v", mine)
	}
	if status, _ := r.ack(t, cid, beta, "fine"); status != http.StatusOK {
		t.Fatalf("delegator ack: %d", status)
	}
	if replies := frameTexts(r.wire.received(alpha.PeerID)); !slices.Equal(replies, []string{"[ack #" + cid + " from @beta] fine"}) {
		t.Fatalf("alpha received %q", replies)
	}
}

func TestDelegateToBusyPeerDefersToItsReminder(t *testing.T) {
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	gamma.Status = proto.StatusBusy
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "review?")
	status, out := r.delegate(t, cid, beta, gamma, "")
	if status != http.StatusOK || out["transport"] != "deferred" {
		t.Fatalf("delegate to busy peer: %d %v", status, out)
	}
	if got := r.wire.received(gamma.PeerID); len(got) != 0 {
		t.Fatalf("a busy peer was interrupted: %v", got)
	}
	if theirs := r.pending(t, gamma, "inbound"); len(theirs) != 1 || theirs[0].CorrelationID != cid {
		t.Fatalf("gamma's reminder = %+v", theirs)
	}
}

func TestConcurrentRepliesReachAskerOnce(t *testing.T) {
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "review?")
	if status, _ := r.delegate(t, cid, beta, gamma, ""); status != http.StatusOK {
		t.Fatal("delegate failed")
	}
	// Hold every reply on the wire, so the racers overlap between reading
	// the open ask and closing it.
	r.wire.mu.Lock()
	r.wire.slow = 50 * time.Millisecond
	r.wire.mu.Unlock()
	const racers = 16
	codes := make(chan int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := r.ack(t, cid, gamma, fmt.Sprintf("reply %d", i))
			codes <- status
		}(i)
	}
	wg.Wait()
	close(codes)
	ok := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict, http.StatusGone:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if replies := r.wire.received(alpha.PeerID); ok != 1 || len(replies) != 1 {
		t.Fatalf("%d acks succeeded and alpha got %d replies, want 1 and 1", ok, len(replies))
	}
}

func TestDelegateRefusals(t *testing.T) {
	alpha, beta, gamma, delta := alphaPeer(), betaPeer(), gammaPeer(), deltaPeer()
	stranger := peerWith("repow-other-eeee", "epsilon", "other", proto.StatusOnline)
	r := newDelegateRig(t, alpha, beta, gamma, delta, stranger)
	cid := r.ask(t, alpha, beta, "review?")

	for _, tc := range []struct {
		name string
		req  DelegateRequest
		want int
	}{
		{"no target", DelegateRequest{FromPeer: strp(string(beta.PeerID))}, http.StatusUnprocessableEntity},
		{"unknown target", DelegateRequest{ToPeer: "nobody", FromPeer: strp(string(beta.PeerID))}, http.StatusNotFound},
		{"to the asker", DelegateRequest{ToPeer: "alpha", FromPeer: strp(string(beta.PeerID))}, http.StatusUnprocessableEntity},
		{"to the holder", DelegateRequest{ToPeer: "beta", FromPeer: strp(string(beta.PeerID))}, http.StatusUnprocessableEntity},
		{"by a non-holder", DelegateRequest{ToPeer: "gamma", FromPeer: strp(string(delta.PeerID))}, http.StatusForbidden},
		{"across circles", DelegateRequest{ToPeer: "epsilon", FromPeer: strp(string(beta.PeerID))}, http.StatusForbidden},
	} {
		if status, out := r.post(t, "/asks/"+cid+"/delegate", tc.req); status != tc.want {
			t.Fatalf("%s: %d %v, want %d", tc.name, status, out, tc.want)
		}
	}
	if ask, _ := r.asks.Get(cid); ask.ToPeerID != beta.PeerID || len(ask.Delegations) != 0 || len(r.wire.received(gamma.PeerID)) != 0 {
		t.Fatalf("a refused delegation changed the ask: %+v", ask)
	}
	if status, _ := r.post(t, "/asks/ask-missing/delegate", DelegateRequest{ToPeer: "gamma"}); status != http.StatusNotFound {
		t.Fatalf("unknown ask: %d", status)
	}

	// A job's dispatch ask stays with its assigned peer.
	jobCid := r.ask(t, alpha, beta, "job prompt")
	if _, err := r.store.CreateWork(context.Background(), state.WorkCreate{Title: "nightly", CorrelationID: &jobCid}); err != nil {
		t.Fatal(err)
	}
	if status, out := r.delegate(t, jobCid, beta, gamma, ""); status != http.StatusConflict || out["detail"].(map[string]any)["error"] != "job_ask" {
		t.Fatalf("job ask: %d %v", status, out)
	}

	if status, _ := r.ack(t, cid, beta, "done"); status != http.StatusOK {
		t.Fatal("ack failed")
	}
	if status, _ := r.delegate(t, cid, beta, gamma, ""); status != http.StatusGone {
		t.Fatalf("closed ask: %d", status)
	}
}
