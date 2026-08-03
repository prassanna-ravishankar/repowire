package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ----------------------------------------------------------------------------
// Fakes. fakeTransport / newRouterWithFake are shared with router_test.go.
// ----------------------------------------------------------------------------

// fakeRegistry satisfies accessRegistry. CheckAccess resolves from/to out of the
// peers map by display_name or peer_id; an unknown target returns checkErr.
type fakeRegistry struct {
	mu         sync.Mutex
	peers      []*proto.Peer
	checkErr   error // forced CheckAccess error (unknown/ambiguous/forbidden)
	events     []recordedEvent
	offlined   []proto.PeerID
	offlineErr error
}

type recordedEvent struct {
	typ     string
	payload map[string]any
}

func (r *fakeRegistry) lookup(name string) *proto.Peer {
	for _, p := range r.peers {
		if string(p.DisplayName) == name || string(p.PeerID) == name {
			return p
		}
	}
	return nil
}

func (r *fakeRegistry) CheckAccess(ctx context.Context, fromPeer, toPeer string, bypassCircle bool, circle *string) (*proto.Peer, *proto.Peer, error) {
	if r.checkErr != nil {
		return nil, nil, r.checkErr
	}
	target := r.lookup(toPeer)
	if target == nil {
		return nil, nil, errors.New("Unknown peer: " + toPeer)
	}
	return r.lookup(fromPeer), target, nil
}

func (r *fakeRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	if p := r.lookup(string(id)); p != nil {
		return p, true
	}
	return nil, false
}

func (r *fakeRegistry) GetAllPeers() []*proto.Peer { return r.peers }

func (r *fakeRegistry) AddEvent(ctx context.Context, typ string, payload map[string]any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{typ: typ, payload: payload})
	return "evt-" + typ
}

func (r *fakeRegistry) MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offlined = append(r.offlined, id)
	return 0, r.offlineErr
}

func (r *fakeRegistry) eventTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.typ
	}
	return out
}

// fakeQueue satisfies queuedDeliveryStore. enqueueNil forces the "queue disabled"
// (cap/ttl <= 0) return so the fail-loud path is exercised.
type fakeQueue struct {
	enqueued   []state.QueuedDelivery
	enqueueNil bool
	enqueueErr error
}

func (q *fakeQueue) EnqueueDelivery(ctx context.Context, d state.QueuedDelivery, ttlSeconds float64, maxPerPeer int, now time.Time) (*state.QueuedDelivery, error) {
	if q.enqueueErr != nil {
		return nil, q.enqueueErr
	}
	if q.enqueueNil {
		return nil, nil
	}
	out := d
	out.DeliveryID = "qd-test"
	q.enqueued = append(q.enqueued, out)
	return &out, nil
}

func peerWith(id, name, circle string, status proto.PeerStatus) *proto.Peer {
	return &proto.Peer{PeerID: proto.PeerID(id), DisplayName: proto.DisplayName(name), Circle: circle, Status: status, Role: proto.RoleAgent}
}

// ----------------------------------------------------------------------------
// Notify
// ----------------------------------------------------------------------------

// TestNotifyTransportDelivered is the happy WS path: the hook acks within the
// window, so the result is sent/delivered/transport_delivered over ws, carrying
// the hook receipt and resolved identity.
func TestNotifyTransportDelivered(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	from := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{from, target}}
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}, ackDelay: 0}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	res, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "hi"})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if res.Status != "sent" || res.DeliveryState != "delivered" || res.Reason != "transport_delivered" {
		t.Fatalf("expected sent/delivered/transport_delivered, got %+v", res)
	}
	if res.Transport != "ws" {
		t.Fatalf("expected ws transport, got %q", res.Transport)
	}
	if res.ToPeerID != target.PeerID || res.FromPeerID == nil || *res.FromPeerID != from.PeerID {
		t.Fatalf("identity not resolved: %+v", res)
	}
	if f.lastTarget != target.PeerID {
		t.Fatalf("notify must route by PeerID, got %s", f.lastTarget)
	}
	if res.HookDelivery["status"] != "injected" {
		t.Fatalf("expected hook receipt echoed, got %+v", res.HookDelivery)
	}
}

// TestNotifyQueuesWhenNoTransport is the no-live-transport fallback: SendNotification
// returns ErrNotConnected, so the peer is marked offline and the message is
// enqueued (queued/queued/queued_delivery) and a notification event is recorded.
func TestNotifyQueuesWhenNoTransport(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	q := &fakeQueue{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, q)

	res, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "later"})
	if err != nil {
		t.Fatalf("queued notify must not error, got %v", err)
	}
	if res.Status != "queued" || res.DeliveryState != "queued" || res.Reason != "queued_delivery" {
		t.Fatalf("expected queued outcome, got %+v", res)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].PeerID != string(target.PeerID) || q.enqueued[0].Kind != state.DeliveryNotify {
		t.Fatalf("expected one notify enqueued for the target, got %+v", q.enqueued)
	}
	if len(reg.offlined) != 1 || reg.offlined[0] != target.PeerID {
		t.Fatalf("expected target marked offline, got %+v", reg.offlined)
	}
	saw := false
	for _, et := range reg.eventTypes() {
		if et == "notification" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected a notification event recorded, got %v", reg.eventTypes())
	}
}

func TestBusyWSRecipientDefersWithoutInjection(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusBusy)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}}
	q := &fakeQueue{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), q)

	notify, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "later"})
	if err != nil || !notify.Queued() || notify.Reason != "recipient_busy" || len(q.enqueued) != 1 {
		t.Fatalf("busy notify = %+v, %v; queue = %+v", notify, err, q.enqueued)
	}
	ask, err := d.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "beta", Text: "question", CorrelationID: "cid-busy"})
	if err != nil || ask.Transport != "deferred" {
		t.Fatalf("busy ask = %+v, %v", ask, err)
	}
	if f.lastTarget != "" || len(reg.offlined) != 0 {
		t.Fatalf("busy peer was injected/offlined: target=%q offline=%v", f.lastTarget, reg.offlined)
	}
}

func TestBusyThreadSteeringRecipientDeliversImmediately(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusBusy)
	target.Metadata = map[string]any{"capabilities": []any{proto.CapThreadSteering}}
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), &fakeQueue{})

	notify, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "now"})
	if err != nil || notify.Queued() || notify.Transport != "ws" {
		t.Fatalf("steerable busy notify = %+v, %v", notify, err)
	}
	ask, err := d.DeliverAsk(context.Background(), DeliverAskParams{FromPeer: "alpha", ToPeer: "beta", Text: "question", CorrelationID: "cid-live"})
	if err != nil || ask.Transport != "ws" {
		t.Fatalf("steerable busy ask = %+v, %v", ask, err)
	}
}

// TestNotifyFailsLoudWhenQueueDisabled: with no queue store wired, a no-transport
// notify must propagate the TransportError (fail loud → 503), never silently
// claim success.
func TestNotifyFailsLoudWhenQueueDisabled(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	_, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "x"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected to propagate when queue disabled, got %v", err)
	}
	// Even with no store, the peer is still marked offline (it had no transport).
	if len(reg.offlined) != 1 {
		t.Fatalf("expected target marked offline, got %+v", reg.offlined)
	}
}

// TestNotifyQueueDisabledByCapFailsLoud: the store is wired but returns (nil,nil)
// because the cap/ttl is <= 0 (nothing durable written) — that too must fail loud.
func TestNotifyQueueDisabledByCapFailsLoud(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	q := &fakeQueue{enqueueNil: true}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, q)

	_, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "x"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected fail-loud when enqueue returns nil, got %v", err)
	}
}

// TestNotifyUnknownTargetPropagatesCheckAccess: CheckAccess error short-circuits
// before any transport work.
func TestNotifyUnknownTargetPropagatesCheckAccess(t *testing.T) {
	reg := &fakeRegistry{}
	f := &fakeTransport{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	if _, err := d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "ghost", Text: "x"}); err == nil {
		t.Fatalf("expected unknown-target error")
	}
	if len(f.sentTargets) != 0 {
		t.Fatalf("must not touch transport on access failure, sent=%v", f.sentTargets)
	}
}

// ----------------------------------------------------------------------------
// DeliverAsk
// ----------------------------------------------------------------------------

// TestDeliverAskHappyPath: WS delivery returns the ws transport and the hook
// receipt.
func TestDeliverAskHappyPath(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}, ackDelay: 0}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), nil)

	res, err := d.DeliverAsk(context.Background(), DeliverAskParams{
		FromPeer: "alpha", ToPeer: "beta", Text: "help?", CorrelationID: "cid-1",
	})
	if err != nil {
		t.Fatalf("deliver ask: %v", err)
	}
	if res.Transport != "ws" || res.HookDelivery["status"] != "injected" {
		t.Fatalf("unexpected ask result: %+v", res)
	}
}

// TestDeliverAskInjectionFailureDoesNotMarkOffline is the fail-loud contract: a
// DeliveryInjectionError (hook reached, pane rejected) propagates unchanged and
// the peer is NOT marked offline (the socket is alive).
func TestDeliverAskInjectionFailureDoesNotMarkOffline(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackFrame: map[string]any{"status": "rejected", "detail": "pane not safe"}, ackDelay: 0}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), nil)

	_, err := d.DeliverAsk(context.Background(), DeliverAskParams{
		FromPeer: "alpha", ToPeer: "beta", Text: "do it", CorrelationID: "cid-2",
	})
	di, ok := AsDeliveryInjection(err)
	if !ok {
		t.Fatalf("expected *DeliveryInjectionError, got %T: %v", err, err)
	}
	if di.Status != "rejected" {
		t.Fatalf("expected rejected status, got %q", di.Status)
	}
	if len(reg.offlined) != 0 {
		t.Fatalf("injection failure on a live socket must NOT mark the peer offline, got %v", reg.offlined)
	}
}

// TestDeliverAskTransportFailureMarksOffline: a genuine ErrNotConnected (no live
// socket) marks the peer offline and propagates.
func TestDeliverAskTransportFailureMarksOffline(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), nil)

	_, err := d.DeliverAsk(context.Background(), DeliverAskParams{
		FromPeer: "alpha", ToPeer: "beta", Text: "x", CorrelationID: "cid-3",
	})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected to propagate, got %v", err)
	}
	if len(reg.offlined) != 1 || reg.offlined[0] != target.PeerID {
		t.Fatalf("genuine transport failure must mark peer offline, got %v", reg.offlined)
	}
}

func TestDeliverAskTransportFailurePreservesPaneRuntime(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	pane := "%9"
	target.PaneID = &pane
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), nil)

	_, err := d.DeliverAsk(context.Background(), DeliverAskParams{
		FromPeer: "alpha", ToPeer: "beta", Text: "x", CorrelationID: "cid-pane",
	})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected to remain fail-loud, got %v", err)
	}
	if len(reg.offlined) != 0 {
		t.Fatalf("transport loss must not demote a pane-backed runtime, got %v", reg.offlined)
	}
}

func TestCompleteACPAskQueuedReplyClosesWithoutPendingDuplicate(t *testing.T) {
	asker := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	responder := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{asker, responder}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	queue := &fakeQueue{}
	asks := NewAskTracker(0)
	cid, err := asks.Register(context.Background(), RegisterAskParams{
		FromPeerID: asker.PeerID, FromPeerName: asker.DisplayName,
		ToPeerID: responder.PeerID, ToPeerName: responder.DisplayName,
	})
	if err != nil {
		t.Fatalf("register ask: %v", err)
	}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, asks, queue)

	reply := "done"
	d.completeACPAsk(context.Background(), cid, &reply, nil)

	if len(queue.enqueued) != 1 {
		t.Fatalf("expected one durable reply, got %d", len(queue.enqueued))
	}
	ask, ok := asks.Get(cid)
	if !ok || !ask.Closed || ask.CloseReason != "ack_with_msg" {
		t.Fatalf("expected closed ack_with_msg ask, got %+v", ask)
	}
	if ask.ReplyText == nil || *ask.ReplyText != reply {
		t.Fatalf("expected captured reply %q, got %+v", reply, ask.ReplyText)
	}
	if ask.PendingReply != nil {
		t.Fatalf("queued reply must not also be stashed, got %q", *ask.PendingReply)
	}
}

// TestMarkOfflineSkipsCliFallback: a repowire_cli_fallback peer is never retired
// on a failed push.
func TestMarkOfflineSkipsCliFallback(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	target.Metadata = map[string]any{"repowire_cli_fallback": true}
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	_, _ = d.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "x"})
	if len(reg.offlined) != 0 {
		t.Fatalf("cli-fallback peer must not be marked offline, got %v", reg.offlined)
	}
}

// ----------------------------------------------------------------------------
// OpenScheduledAsk
// ----------------------------------------------------------------------------

// TestOpenScheduledAskRollsBackOnSendFailure: when delivery fails, the registered
// ask is closed as send_failed (no dangling open ask).
func TestOpenScheduledAskRollsBackOnSendFailure(t *testing.T) {
	target := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{target}}
	f := &fakeTransport{ackErr: ErrNotConnected}
	asks := NewAskTracker(0)
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil)

	_, err := d.OpenScheduledAsk(context.Background(), "alpha", "beta", "scheduled", nil, "pull")
	if err == nil {
		t.Fatalf("expected send failure to propagate")
	}
	if asks.OpenCount() != 0 {
		t.Fatalf("failed scheduled ask must be rolled back (closed), open=%d", asks.OpenCount())
	}
}

// TestOpenScheduledAskRequiresTracker: without an AskTracker the helper errors.
func TestOpenScheduledAskRequiresTracker(t *testing.T) {
	reg := &fakeRegistry{}
	f := &fakeTransport{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)
	if _, err := d.OpenScheduledAsk(context.Background(), "a", "b", "x", nil, "push"); err == nil {
		t.Fatalf("expected error when AskTracker is nil")
	}
}

// ----------------------------------------------------------------------------
// Broadcast
// ----------------------------------------------------------------------------

// TestBroadcastCircleGating: an agent sender reaches only same-circle peers; a
// cross-circle peer is excluded, and the sender excludes itself.
func TestBroadcastCircleGating(t *testing.T) {
	sender := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	same := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	other := peerWith("repow-other-cccc", "gamma", "other", proto.StatusOnline)
	reg := &fakeRegistry{peers: []*proto.Peer{sender, same, other}}
	f := &fakeTransport{
		sessions:  []proto.PeerID{sender.PeerID, same.PeerID, other.PeerID},
		connected: map[proto.PeerID]bool{},
	}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	sent, failed := d.Broadcast(context.Background(), "alpha", "hello all", nil, false)
	if len(failed) != 0 {
		t.Fatalf("no recipient should fail, got %+v", failed)
	}
	if len(sent) != 1 || sent[0] != "beta" {
		t.Fatalf("expected only same-circle beta, got %v", sent)
	}
	// The router only sends to non-excluded sessions; sender + cross-circle peer
	// must NOT have received the frame.
	for _, target := range f.sentTargets {
		if target == sender.PeerID || target == other.PeerID {
			t.Fatalf("broadcast leaked to %s (sender/cross-circle)", target)
		}
	}
	// A broadcast event is always recorded.
	saw := false
	for _, et := range reg.eventTypes() {
		if et == "broadcast" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected a broadcast event recorded, got %v", reg.eventTypes())
	}
}

// TestBroadcastDefersPendingFirstTurnPeer: a still-seeding WS peer is delivered
// via the deferred (seed-gated) goroutine and excluded from the synchronous
// fanout, so it never receives twice. It is still reported as delivered.
func TestBroadcastDefersPendingFirstTurnPeer(t *testing.T) {
	sender := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	seeding := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	seeding.TurnState = proto.TurnPendingFirstTurn
	reg := &fakeRegistry{peers: []*proto.Peer{sender, seeding}}
	f := &fakeTransport{
		sessions:  []proto.PeerID{sender.PeerID, seeding.PeerID},
		connected: map[proto.PeerID]bool{},
	}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	sent, _ := d.Broadcast(context.Background(), "alpha", "hello", nil, false)
	if len(sent) != 1 || sent[0] != "beta" {
		t.Fatalf("seeding peer must still be reported delivered (deferred), got %v", sent)
	}
	// The seeding peer was excluded from the synchronous router fanout: the only
	// way it gets the frame is the deferred goroutine. The seed never settles
	// here (turn_state stays pending), so within a short wait the synchronous
	// fanout must not have targeted it.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, target := range f.sentTargets {
		if target == seeding.PeerID {
			t.Fatalf("seeding peer must be deferred, not in the synchronous fanout")
		}
	}
}

// ----------------------------------------------------------------------------
// Close (shutdown correctness)
// ----------------------------------------------------------------------------

// TestDeliveryClose_UnblocksSeedGate is the load-bearing shutdown-correctness
// check: the seeding peer never leaves pending_first_turn in this test, so the
// deferBroadcastUntilSeedSettled goroutine would otherwise be parked in
// awaitSeedSettled's poll for up to seedSettleWait (25s). Close must return in
// well under that. A mis-wired closeCh (or a plain WaitGroup join with no early-
// exit signal) would still pass a "Close eventually returns" test — it would
// just take the full 25s — so the assertion is on wall-clock time.
func TestDeliveryClose_UnblocksSeedGate(t *testing.T) {
	sender := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	seeding := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	seeding.TurnState = proto.TurnPendingFirstTurn
	reg := &fakeRegistry{peers: []*proto.Peer{sender, seeding}}
	f := &fakeTransport{
		sessions:  []proto.PeerID{sender.PeerID, seeding.PeerID},
		connected: map[proto.PeerID]bool{},
	}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)

	// Broadcast excludes the seeding peer from the synchronous fanout and defers
	// it behind the seed gate (deferBroadcastUntilSeedSettled), exactly as
	// TestBroadcastDefersPendingFirstTurnPeer exercises.
	d.Broadcast(context.Background(), "alpha", "hello", nil, false)

	done := make(chan struct{})
	go func() {
		d.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Close did not return within 3s; the deferred seed-gate goroutine was not unblocked")
	}
}
