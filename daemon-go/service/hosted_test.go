package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// fakeHostedRuntime records submissions and settles each operation when the
// test sends its outcome. waitErrs are returned, in order, before an outcome.
type fakeHostedRuntime struct {
	mu        sync.Mutex
	created   []string
	submitted map[string]string
	sources   map[string]HostedSource
	outcomes  map[string]chan HostedResult
	waitErrs  []error
	submitErr error
	createErr error
}

func newFakeHostedRuntime() *fakeHostedRuntime {
	return &fakeHostedRuntime{submitted: map[string]string{}, sources: map[string]HostedSource{}, outcomes: map[string]chan HostedResult{}}
}

func (f *fakeHostedRuntime) outcome(op string) chan HostedResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes[op] == nil {
		f.outcomes[op] = make(chan HostedResult, 1)
	}
	return f.outcomes[op]
}

func (f *fakeHostedRuntime) CreateAgent(_ context.Context, id string, _ *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, id)
	return f.createErr
}

func (f *fakeHostedRuntime) Submit(_ context.Context, _, op, text string, source HostedSource) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return false, f.submitErr
	}
	_, seen := f.submitted[op]
	f.submitted[op], f.sources[op] = text, source
	return !seen, nil
}

func (f *fakeHostedRuntime) Wait(ctx context.Context, _, op string) (HostedResult, error) {
	f.mu.Lock()
	if len(f.waitErrs) > 0 {
		err := f.waitErrs[0]
		f.waitErrs = f.waitErrs[1:]
		f.mu.Unlock()
		return HostedResult{}, err
	}
	f.mu.Unlock()
	select {
	case result := <-f.outcome(op):
		return result, nil
	case <-ctx.Done():
		return HostedResult{}, ctx.Err()
	}
}

// fakeHostedRegistry records the status changes the hosted service makes.
type fakeHostedRegistry struct {
	mu       sync.Mutex
	statuses []proto.PeerStatus
	params   []peer.AllocateParams
	offline  []proto.PeerID
}

func (r *fakeHostedRegistry) AllocateAndRegister(_ context.Context, params peer.AllocateParams) (proto.PeerID, proto.DisplayName, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.params = append(r.params, params)
	return "repow-default-h0st", "helper-hosted", nil
}

func (r *fakeHostedRegistry) UpdateStatus(_ context.Context, _ proto.PeerID, status proto.PeerStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, status)
	return nil
}

func (r *fakeHostedRegistry) UpdateTurnState(context.Context, proto.PeerID, proto.TurnState) {}

func (r *fakeHostedRegistry) MarkOffline(_ context.Context, id proto.PeerID, _ bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offline = append(r.offline, id)
	return 0, nil
}

func (r *fakeHostedRegistry) lastStatus() proto.PeerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.statuses) == 0 {
		return ""
	}
	return r.statuses[len(r.statuses)-1]
}

func newHostedStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type hostedFixture struct {
	asks     *AskTracker
	runtime  *fakeHostedRuntime
	hostReg  *fakeHostedRegistry
	hosted   *Hosted
	delivery *PeerDelivery
	wire     *fakeTransport
	store    *state.Store
	asker    *proto.Peer
	target   *proto.Peer
}

func newHostedFixture(t *testing.T) *hostedFixture {
	t.Helper()
	asker := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	target := peerWith("repow-default-h0st", "helper-hosted", "default", proto.StatusOnline)
	target.Backend = proto.AgentHosted
	f := &hostedFixture{asks: NewAskTracker(0), runtime: newFakeHostedRuntime(), hostReg: &fakeHostedRegistry{},
		wire: &fakeTransport{ackFrame: map[string]any{"status": "injected"}}, store: newHostedStore(t), asker: asker, target: target}
	f.hosted = NewHosted(f.hostReg, f.runtime, f.store, "test")
	f.hosted.retry = time.Millisecond
	t.Cleanup(f.hosted.Close)
	reg := &fakeRegistry{peers: []*proto.Peer{asker, target}}
	f.delivery = NewPeerDelivery(reg, newRouterWithFake(f.wire), f.wire, f.asks, nil).WithHosted(f.hosted)
	return f
}

func (f *hostedFixture) ask(t *testing.T) string {
	t.Helper()
	cid, err := f.asks.Register(context.Background(), RegisterAskParams{
		FromPeerID: f.asker.PeerID, FromPeerName: f.asker.DisplayName, ToPeerID: f.target.PeerID, ToPeerName: f.target.DisplayName,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "helper-hosted", Text: "what is 6*7?", CorrelationID: cid})
	if err != nil {
		t.Fatalf("deliver ask: %v", err)
	}
	if res.Transport != "hosted" || res.HookDelivery["status"] != "accepted" {
		t.Fatalf("unexpected ask result: %+v", res)
	}
	return cid
}

func waitClosed(t *testing.T, asks *AskTracker, cid string) *Ask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ask, ok := asks.Get(cid); ok && ask.Closed {
			return ask
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ask %s never closed", cid)
	return nil
}

func TestHostedAskClosesWithRuntimeAnswer(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)

	text := f.runtime.submitted[cid]
	if !strings.Contains(text, `<peer-message from="@alpha"`) || !strings.Contains(text, `type="ask"`) || !strings.Contains(text, "what is 6*7?") {
		t.Fatalf("submission must carry the untrusted peer framing, got %q", text)
	}
	if source := f.runtime.sources[cid]; source.From != "alpha" || source.CorrelationID != cid || source.Type != "ask" {
		t.Fatalf("unexpected source %+v", source)
	}
	if f.hostReg.lastStatus() != proto.StatusBusy {
		t.Fatalf("peer must be busy while the runtime works, got %q", f.hostReg.lastStatus())
	}

	f.runtime.outcome(cid) <- HostedResult{Status: "done", Text: "42"}
	ask := waitClosed(t, f.asks, cid)
	if ask.CloseReason != "ack_with_msg" || ask.ReplyText == nil || *ask.ReplyText != "42" {
		t.Fatalf("expected ack_with_msg with reply 42, got %+v", ask)
	}
	if f.wire.lastTarget != f.asker.PeerID {
		t.Fatalf("reply must reach the asker, went to %q", f.wire.lastTarget)
	}
	f.hosted.wg.Wait()
	if f.hostReg.lastStatus() != proto.StatusOnline {
		t.Fatalf("peer must return online once idle, got %q", f.hostReg.lastStatus())
	}
	ops, _ := f.store.ListOperations(context.Background(), hostedDeliveryKind, "completed")
	if len(ops) != 1 {
		t.Fatalf("expected one settled operation, got %d", len(ops))
	}
}

func TestHostedUnansweredFailsTheAskLoudly(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	f.runtime.outcome(cid) <- HostedResult{Status: "unanswered", Reason: "run_failed"}
	ask := waitClosed(t, f.asks, cid)
	if ask.CloseReason != "send_failed" {
		t.Fatalf("an unanswered run must close send_failed, got %q", ask.CloseReason)
	}
}

func TestHostedWaitReconnectsAfterNetworkFailure(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.waitErrs = []error{errors.New("connection reset"), context.DeadlineExceeded}
	cid := f.ask(t)
	f.runtime.outcome(cid) <- HostedResult{Status: "done", Text: "ok"}
	if ask := waitClosed(t, f.asks, cid); ask.CloseReason != "ack_with_msg" {
		t.Fatalf("expected the reconnected wait to settle the ask, got %+v", ask)
	}
}

func TestHostedSubmitFailureFailsLoud(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.submitErr = &HostedError{Status: 404, Message: "Agent not found"}
	_, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "helper-hosted", Text: "hi", CorrelationID: "ask-1"})
	if err == nil || !strings.Contains(err.Error(), "Agent not found") {
		t.Fatalf("expected the runtime refusal to propagate, got %v", err)
	}
	if f.hostReg.lastStatus() != "" {
		t.Fatalf("a refused submission must not mark the peer busy, got %q", f.hostReg.lastStatus())
	}
}

func TestHostedRestoreResumesUnsettledDelivery(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	f.hosted.Close() // the daemon stops while the runtime is still working

	runtime := newFakeHostedRuntime()
	next := NewHosted(f.hostReg, runtime, f.store, "test")
	t.Cleanup(next.Close)
	settled := make(chan HostedDelivery, 1)
	next.OnSettled(func(_ context.Context, d HostedDelivery, result HostedResult, _ error) {
		if result.Text == "42" {
			settled <- d
		}
	})
	next.Restore(context.Background())
	runtime.outcome(cid) <- HostedResult{Status: "done", Text: "42"}
	select {
	case d := <-settled:
		if d.CorrelationID != cid || d.FromPeerID != string(f.asker.PeerID) {
			t.Fatalf("resumed delivery lost its thread: %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restored daemon never settled the unsettled delivery")
	}
}

func TestHostedSpawnRetiresPeerWhenRuntimeRefuses(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.createErr = &HostedError{Status: 409, Message: "Agent already exists with a different workspace"}
	if _, _, err := f.hosted.Spawn(context.Background(), HostedSpawnParams{Path: "helper", Circle: "default"}); err == nil {
		t.Fatal("expected spawn to fail")
	}
	if len(f.hostReg.offline) != 1 {
		t.Fatalf("a refused spawn must retire the registered peer, got %v", f.hostReg.offline)
	}
	params := f.hostReg.params[0]
	if params.Backend != proto.AgentHosted || params.Metadata["in_process"] != true || params.Provenance.Source != proto.SourceHosted {
		t.Fatalf("unexpected registration %+v", params)
	}
}

func TestHostedPullAskKeepsReplyOnTheAsk(t *testing.T) {
	f := newHostedFixture(t)
	cid, err := f.asks.Register(context.Background(), RegisterAskParams{
		FromPeerID: "relay-client", FromPeerName: "relay-client", ToPeerID: f.target.PeerID, ToPeerName: f.target.DisplayName,
		ReplyDelivery: "pull",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "relay-client", ToPeer: "helper-hosted", Text: "hi", CorrelationID: cid}); err != nil {
		t.Fatal(err)
	}
	f.runtime.outcome(cid) <- HostedResult{Status: "done", Text: "hello"}
	ask := waitClosed(t, f.asks, cid)
	if ask.CloseReason != "ack_with_msg" || ask.ReplyText == nil || *ask.ReplyText != "hello" {
		t.Fatalf("pull reply must stay on the ask, got %+v", ask)
	}
	if f.wire.lastTarget != "" {
		t.Fatalf("a pull reply must not be pushed, sent to %q", f.wire.lastTarget)
	}
}

func TestHostedReplyAfterRestartQueuesForAbsentAsker(t *testing.T) {
	f := newHostedFixture(t)
	queue := &fakeQueue{}
	f.delivery.store = queue
	// The tracker lost the ask and the asker has not re-registered.
	f.delivery.completeHosted(context.Background(), HostedDelivery{Kind: "ask", CorrelationID: "ask-gone",
		FromPeerID: "repow-default-gone", FromPeerName: "gone", ToPeerID: string(f.target.PeerID), ToPeerName: string(f.target.DisplayName)},
		HostedResult{Status: "done", Text: "late answer"}, nil)
	if len(queue.enqueued) != 1 || queue.enqueued[0].PeerID != "repow-default-gone" || !strings.Contains(queue.enqueued[0].Text, "[ack #ask-gone from @helper-hosted] late answer") {
		t.Fatalf("expected the reply queued for the asker, got %+v", queue.enqueued)
	}
}
