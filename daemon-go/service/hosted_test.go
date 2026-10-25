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

// fakeHostedRuntime records agents and submissions; tests settle a
// submission by reporting it, as the gateway does.
type fakeHostedRuntime struct {
	mu        sync.Mutex
	created   []string
	submitted map[string]string
	sources   map[string]HostedSource
	retired   []string
	submitErr error
	createErr error
	retireErr error
	// onSubmit runs after a submission is recorded, before Submit returns,
	// as a runtime that settles fast would.
	onSubmit func(op string)
}

func newFakeHostedRuntime() *fakeHostedRuntime {
	return &fakeHostedRuntime{submitted: map[string]string{}, sources: map[string]HostedSource{}}
}

func (f *fakeHostedRuntime) CreateAgent(_ context.Context, id, _ string, _ *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, id)
	return f.createErr
}

func (f *fakeHostedRuntime) RetireAgent(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired = append(f.retired, id)
	return f.retireErr
}

func (f *fakeHostedRuntime) Submit(_ context.Context, _, op, text string, source HostedSource) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return false, f.submitErr
	}
	_, seen := f.submitted[op]
	f.submitted[op], f.sources[op] = text, source
	if f.onSubmit != nil {
		f.mu.Unlock()
		f.onSubmit(op)
		f.mu.Lock()
	}
	return !seen, nil
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

// report settles a submission the way a gateway report does.
func (f *hostedFixture) report(op string, result HostedResult) {
	if err := f.hosted.Report(context.Background(), string(f.target.PeerID), op, result); err != nil {
		panic(err)
	}
}

// openDeliveries counts hosted deliveries not yet settled.
func (f *hostedFixture) openDeliveries(t *testing.T) int {
	t.Helper()
	open := 0
	for _, status := range []string{"queued", "running"} {
		ops, err := f.store.ListOperations(context.Background(), hostedDeliveryKind, status)
		if err != nil {
			t.Fatal(err)
		}
		open += len(ops)
	}
	return open
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

	f.report(cid, HostedResult{Status: "done", Text: "42"})
	ask := waitClosed(t, f.asks, cid)
	if ask.CloseReason != "ack_with_msg" || ask.ReplyText == nil || *ask.ReplyText != "42" {
		t.Fatalf("expected ack_with_msg with reply 42, got %+v", ask)
	}
	if f.wire.lastTarget != f.asker.PeerID {
		t.Fatalf("reply must reach the asker, went to %q", f.wire.lastTarget)
	}
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
	f.report(cid, HostedResult{Status: "unanswered", Reason: "run_failed"})
	ask := waitClosed(t, f.asks, cid)
	if ask.CloseReason != "send_failed" {
		t.Fatalf("an unanswered run must close send_failed, got %q", ask.CloseReason)
	}
}

// The runtime resends a report until it is acknowledged, so a report for a
// settled operation must change nothing.
func TestHostedRepeatedReportSettlesOnce(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	settled := 0
	f.hosted.OnSettled("count", func(context.Context, HostedDelivery, HostedResult) error { settled++; return nil })
	f.report(cid, HostedResult{Status: "done", Text: "ok"})
	f.report(cid, HostedResult{Status: "done", Text: "ok"})
	if settled != 1 || f.hostReg.lastStatus() != proto.StatusOnline {
		t.Fatalf("settled %d times, status %q", settled, f.hostReg.lastStatus())
	}
}

func TestHostedSubmitFailureFailsLoud(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.submitErr = &HostedError{Status: 404, Message: "Agent not found"}
	_, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "helper-hosted", Text: "hi", CorrelationID: "ask-1"})
	if err == nil || !strings.Contains(err.Error(), "Agent not found") {
		t.Fatalf("expected the runtime refusal to propagate, got %v", err)
	}
	if got := f.hostReg.lastStatus(); got == proto.StatusBusy {
		t.Fatalf("a refused submission must not leave the peer busy, got %q", got)
	}
}

func TestHostedRestoreSettlesAReportAfterRestart(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	f.hosted.Close() // the daemon stops while the runtime is still working

	reg := &fakeHostedRegistry{}
	next := NewHosted(reg, newFakeHostedRuntime(), f.store, "test")
	t.Cleanup(next.Close)
	var settled []HostedDelivery
	next.OnSettled("record", func(_ context.Context, d HostedDelivery, _ HostedResult) error {
		settled = append(settled, d)
		return nil
	})
	next.Restore(context.Background())
	if reg.lastStatus() != proto.StatusBusy {
		t.Fatalf("a peer with unsettled work must restore busy, got %q", reg.lastStatus())
	}
	next.Report(context.Background(), string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "42"})
	if len(settled) != 1 || settled[0].CorrelationID != cid || settled[0].FromPeerID != string(f.asker.PeerID) || !strings.Contains(settled[0].Text, "what is 6*7?") {
		t.Fatalf("restored delivery lost its thread: %+v", settled)
	}
	if reg.lastStatus() != proto.StatusOnline {
		t.Fatalf("peer must return online once settled, got %q", reg.lastStatus())
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
	f.report(cid, HostedResult{Status: "done", Text: "hello"})
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
		HostedResult{Status: "done", Text: "late answer"})
	if len(queue.enqueued) != 1 || queue.enqueued[0].PeerID != "repow-default-gone" || !strings.Contains(queue.enqueued[0].Text, "[ack #ask-gone from @helper-hosted] late answer") {
		t.Fatalf("expected the reply queued for the asker, got %+v", queue.enqueued)
	}
}

// Retire refuses the runtime first and keeps the peer when that fails; an
// agent the runtime no longer knows is still retired locally.
func TestHostedRetireIsRemoteFirst(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.retireErr = &HostedError{Status: 502, Message: "upstream down"}
	if err := f.hosted.Retire(context.Background(), f.target.PeerID); err == nil || len(f.hostReg.offline) != 0 {
		t.Fatalf("a failed remote retire must keep the peer: err=%v offline=%v", err, f.hostReg.offline)
	}
	f.runtime.retireErr = &HostedError{Status: 404, Message: "Agent not found"}
	if err := f.hosted.Retire(context.Background(), f.target.PeerID); err != nil || len(f.hostReg.offline) != 1 {
		t.Fatalf("an unknown remote agent must still retire locally: err=%v offline=%v", err, f.hostReg.offline)
	}
}

// Observers run before the delivery turns terminal: a settlement cut short
// leaves the delivery open and unacknowledged, and the resent report settles
// it, keeping the runtime's answer.
func TestHostedReportReplaysWhenSettlementIsInterrupted(t *testing.T) {
	f := newHostedFixture(t)
	stopped, stop := context.WithCancel(context.Background())
	seen := 0
	f.hosted.OnSettled("probe", func(context.Context, HostedDelivery, HostedResult) error {
		seen++
		if f.openDeliveries(t) != 1 {
			t.Error("an observer must run while the delivery is still open")
		}
		stop() // the daemon stops mid-settlement
		return nil
	})
	cid := f.ask(t)
	if err := f.hosted.Report(stopped, string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "42"}); err == nil {
		t.Fatal("an interrupted settlement must not be acknowledged")
	}
	if f.openDeliveries(t) != 1 {
		t.Fatal("an interrupted settlement must leave the delivery open")
	}
	f.report(cid, HostedResult{Status: "done", Text: "42"})
	if seen != 2 || f.openDeliveries(t) != 0 {
		t.Fatalf("the resent report must settle the delivery: observers=%d open=%d", seen, f.openDeliveries(t))
	}
	ops, err := f.store.ListOperations(context.Background(), hostedDeliveryKind, "completed")
	if err != nil || len(ops) != 1 || ops[0].Result["text"] != "42" {
		t.Fatalf("the settled delivery must keep the report, got %+v (%v)", ops, err)
	}
	if ask := waitClosed(t, f.asks, cid); ask.ReplyText == nil || *ask.ReplyText != "42" {
		t.Fatalf("the ask must close with the answer once, got %+v", ask)
	}
}

// A report that cannot be looked up is not acknowledged, so the runtime keeps it.
func TestHostedReportFailsWhenStorageFails(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	_ = f.store.Close()
	if err := f.hosted.Report(context.Background(), string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "42"}); err == nil {
		t.Fatal("a report the daemon could not look up must return an error")
	}
}

// A report that lands while Submit is still returning leaves the peer idle,
// and a duplicate submission does not hold it busy.
func TestHostedFastReportAndDuplicateLeavePeerIdle(t *testing.T) {
	f := newHostedFixture(t)
	f.runtime.onSubmit = func(op string) { f.report(op, HostedResult{Status: "done", Text: "fast"}) }
	cid := f.ask(t)
	if got := f.hostReg.lastStatus(); got != proto.StatusOnline {
		t.Fatalf("a fast report must leave the peer online, got %q", got)
	}
	f.runtime.onSubmit = nil
	if _, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "helper-hosted", Text: "again", CorrelationID: cid}); err != nil {
		t.Fatal(err)
	}
	if got := f.hostReg.lastStatus(); got != proto.StatusOnline || f.openDeliveries(t) != 0 {
		t.Fatalf("a duplicate submission must settle at once: status=%q open=%d", got, f.openDeliveries(t))
	}
}

// A resend of a delivery still open reuses its record, so the runtime's one
// report settles the only record and the peer goes idle.
func TestHostedResendOfAnOpenDeliveryReusesItsRecord(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	if _, err := f.delivery.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "helper-hosted", Text: "again", CorrelationID: cid}); err != nil {
		t.Fatal(err)
	}
	if open := f.openDeliveries(t); open != 1 {
		t.Fatalf("a resend must reuse the open record, got %d open", open)
	}
	f.report(cid, HostedResult{Status: "done", Text: "42"})
	if got := f.hostReg.lastStatus(); got != proto.StatusOnline || f.openDeliveries(t) != 0 {
		t.Fatalf("the report must settle the only record: status=%q open=%d", got, f.openDeliveries(t))
	}
}

// A failing observer leaves the report unacknowledged; the resent report
// runs only the observers that had not succeeded, so a reply already sent is
// not sent again.
func TestHostedReportResumesAtTheFailedObserver(t *testing.T) {
	f := newHostedFixture(t)
	first, flaky := 0, 0
	f.hosted.OnSettled("first", func(context.Context, HostedDelivery, HostedResult) error { first++; return nil })
	f.hosted.OnSettled("flaky", func(context.Context, HostedDelivery, HostedResult) error {
		if flaky++; flaky == 1 {
			return errors.New("store unavailable")
		}
		return nil
	})
	cid := f.ask(t)
	if err := f.hosted.Report(context.Background(), string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "42"}); err == nil {
		t.Fatal("a failed observer must leave the report unacknowledged")
	}
	if f.openDeliveries(t) != 1 {
		t.Fatal("a failed observer must leave the delivery open")
	}
	f.report(cid, HostedResult{Status: "done", Text: "42"})
	if first != 1 || flaky != 2 || f.openDeliveries(t) != 0 {
		t.Fatalf("the resend must resume at the failed observer: first=%d flaky=%d open=%d", first, flaky, f.openDeliveries(t))
	}
}

// A reply that cannot be held for an absent asker fails its observer, so the
// report is resent rather than dropped.
func TestHostedReplyQueueFailureIsResent(t *testing.T) {
	f := newHostedFixture(t)
	f.delivery.store = &fakeQueue{enqueueErr: errors.New("disk full")}
	err := f.delivery.completeHosted(context.Background(), HostedDelivery{Kind: "ask", CorrelationID: "ask-gone",
		FromPeerID: "repow-default-gone", FromPeerName: "gone", ToPeerID: string(f.target.PeerID), ToPeerName: string(f.target.DisplayName)},
		HostedResult{Status: "done", Text: "late answer"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a reply the queue refused must fail the observer, got %v", err)
	}
}

// A reply the asker could not take, held only in memory, fails the reply
// step so the report is resent; it stays on the ask meanwhile.
func TestHostedReplyHeldOnlyInMemoryIsResent(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	f.wire.ackErr, f.wire.sendErr = ErrNotConnected, ErrNotConnected
	if err := f.hosted.Report(context.Background(), string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "42"}); err == nil {
		t.Fatal("a reply held only in memory must leave the report unacknowledged")
	}
	if ask, ok := f.asks.Get(cid); !ok || ask.Closed || ask.PendingReply == nil {
		t.Fatalf("the reply must wait on the open ask, got %+v", ask)
	}
	if f.openDeliveries(t) != 1 {
		t.Fatal("the delivery must stay open for the resend")
	}
}

func (f *fakeHostedRuntime) submittedOp(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.submitted[op]
	return ok
}

func (h *Hosted) inflightFor(id proto.PeerID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[id]
}

// Restore submits each unsettled delivery again: one the runtime never saw
// (the daemon stopped between recording and submitting) reaches it and
// settles on its report, and one the runtime already holds is not counted
// twice.
func TestHostedRestoreResubmitsUnsettledDeliveries(t *testing.T) {
	f := newHostedFixture(t)
	held := f.ask(t)
	lost, _, err := f.hosted.record(context.Background(), map[string]any{
		"agent_id": string(f.target.PeerID), "operation_id": "notify-lost", "kind": "notify",
		"from_peer_id": string(f.asker.PeerID), "from_peer_name": "alpha",
		"to_peer_id": string(f.target.PeerID), "to_peer_name": "helper-hosted", "text": "never submitted",
	})
	if err != nil || lost == "" {
		t.Fatalf("record: %q %v", lost, err)
	}
	f.hosted.Close()

	runtime := newFakeHostedRuntime()
	runtime.submitted[held] = "already held"
	reg := &fakeHostedRegistry{}
	next := NewHosted(reg, runtime, f.store, "test")
	t.Cleanup(next.Close)
	next.Restore(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for !runtime.submittedOp("notify-lost") {
		if time.Now().After(deadline) {
			t.Fatal("restore did not resubmit the delivery the runtime never saw")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := next.inflightFor(f.target.PeerID); got != 2 {
		t.Fatalf("each unsettled delivery counts once, got %d", got)
	}
	for _, op := range []string{held, "notify-lost"} {
		if err := next.Report(context.Background(), string(f.target.PeerID), op, HostedResult{Status: "done", Text: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if reg.lastStatus() != proto.StatusOnline || f.openDeliveries(t) != 0 {
		t.Fatalf("both deliveries must settle: status=%q open=%d", reg.lastStatus(), f.openDeliveries(t))
	}
}

// A resubmission that fails does not settle the delivery: the runtime may
// hold the original, and its genuine report closes the ask with the real
// reply.
func TestHostedRestoreLeavesAFailedResubmissionForItsReport(t *testing.T) {
	f := newHostedFixture(t)
	cid := f.ask(t)
	f.hosted.Close()

	runtime := newFakeHostedRuntime()
	runtime.submitErr = &HostedError{Status: 503, Message: "Service Unavailable"}
	reg := &fakeHostedRegistry{}
	next := NewHosted(reg, runtime, f.store, "test")
	t.Cleanup(next.Close)
	next.OnSettled("reply", f.delivery.completeHosted)
	next.Restore(context.Background())
	next.wg.Wait() // the resubmission has run and failed
	if f.openDeliveries(t) != 1 || reg.lastStatus() != proto.StatusBusy {
		t.Fatalf("a failed resubmission must leave the delivery open and its peer busy: open=%d status=%q", f.openDeliveries(t), reg.lastStatus())
	}
	if err := next.Report(context.Background(), string(f.target.PeerID), cid, HostedResult{Status: "done", Text: "the real answer"}); err != nil {
		t.Fatal(err)
	}
	if ask := waitClosed(t, f.asks, cid); ask.CloseReason != "ack_with_msg" || ask.ReplyText == nil || *ask.ReplyText != "the real answer" {
		t.Fatalf("the genuine report must close the ask with its reply, got %+v", ask)
	}
}
