package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// PeerDelivery — application service that composes registry access-check +
// transport choice (ACP-before-WS) + ask/notify lifecycle + queued-delivery
// fallback. Port of repowire/daemon/peer_delivery.py (PeerDeliveryService).
//
// The WS path and experiment-gated ACP subprocess path are both live. ACP is
// selected only when Transport.ACPRoute validates a target's route metadata;
// every other target falls through to WS.
//
// The router (*MessageRouter) speaks only WS; transport CHOICE lives here. The
// AskTracker owns ask lifecycle; PeerDelivery delivers an already-registered ask
// and, for ACP completion, stashes/redelivers replies.
// ============================================================================

// defaultQueueTTLSeconds / defaultQueueMax mirror the Python QueuedDelivery
// store defaults (config.queued_delivery_ttl_seconds / max_per_peer). The daemon
// overrides them via WithQueueConfig; these are the fallback.
const (
	defaultQueueTTLSeconds = 24 * 60 * 60.0 // 24h
	defaultQueueMax        = 50
)

// seedSettleWait / seedSettlePoll mirror seed_gate.py: hold a WS pane injection
// while the recipient is still pending_first_turn, bounded so a seed that never
// settles cannot wedge delivery forever (a possible interleave beats a stall).
const (
	seedSettleWait = 25 * time.Second
	seedSettlePoll = 500 * time.Millisecond
)

// accessRegistry is the subset of *peer.Registry that PeerDelivery calls.
//
// CheckAccess resolves+authorizes a (from,to) pair (the Python ValueError →
// unknown/ambiguous/forbidden is returned as a non-nil error). The rest mirror
// the registry methods the Tier-1 routes use.
//
// A small adapter makes *peer.Registry satisfy this context-aware seam. Keeping
// it narrow also keeps delivery tests hermetic.
type accessRegistry interface {
	// CheckAccess returns (from, to, err). from is nil for an unknown sender
	// (Python notify behavior: unresolved senders proceed). A non-nil err is the
	// unknown-target / circle-violation / ambiguous-name rejection.
	CheckAccess(ctx context.Context, fromPeer, toPeer string, bypassCircle bool, circle *string) (from, to *proto.Peer, err error)
	GetPeer(id proto.PeerID) (*proto.Peer, bool)
	GetAllPeers() []*proto.Peer
	AddEvent(ctx context.Context, typ string, payload map[string]any) (eventID string)
	MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error)
}

// queuedDeliveryStore is the durable fallback queue. *state.Store satisfies it.
// When nil, a no-live-transport notify fails loud (returns the TransportError)
// instead of silently dropping.
type queuedDeliveryStore interface {
	EnqueueDelivery(ctx context.Context, d state.QueuedDelivery, ttlSeconds float64, maxPerPeer int, now time.Time) (*state.QueuedDelivery, error)
}

// PeerDelivery coordinates peer-to-peer delivery across WS and ACP. Mirrors
// PeerDeliveryService.
type PeerDelivery struct {
	reg       accessRegistry
	router    *MessageRouter
	transport Transport
	asks      *AskTracker
	store     queuedDeliveryStore
	ops       *state.Store

	queueTTLSeconds float64
	queueMax        int
	recall          config.OrchestratorRecallConfig

	// closeMu/closed/wg/closeCh track the deferBroadcastUntilSeedSettled
	// goroutines so Close can join them without waiting out a 25s seed-gate
	// poll. See Close and awaitSeedSettled.
	closeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	closeCh chan struct{}
}

func (d *PeerDelivery) WithOrchestratorRecall(settings config.OrchestratorRecallConfig) *PeerDelivery {
	d.recall = settings
	return d
}

// NewPeerDelivery wires the delivery service. store may be nil (queued-delivery
// fallback disabled → no-live-transport is a fail-loud error). asks may be nil
// (the scheduled-ask helper then errors).
//
// reg and store use the narrow seams above so tests can provide in-memory fakes.
func NewPeerDelivery(reg accessRegistry, router *MessageRouter, transport Transport, asks *AskTracker, store queuedDeliveryStore) *PeerDelivery {
	return &PeerDelivery{
		reg:             reg,
		router:          router,
		transport:       transport,
		asks:            asks,
		store:           store,
		queueTTLSeconds: defaultQueueTTLSeconds,
		queueMax:        defaultQueueMax,
		closeCh:         make(chan struct{}),
	}
}

// WithQueueConfig overrides the queued-delivery ttl/cap (the daemon supplies its
// configured values). Returns the receiver for chaining.
func (d *PeerDelivery) WithQueueConfig(ttlSeconds float64, maxPerPeer int) *PeerDelivery {
	d.queueTTLSeconds = ttlSeconds
	d.queueMax = maxPerPeer
	return d
}

// WithOperationStore enables durable ACP-ask recovery. The state store is already
// the daemon's operation and queued-delivery owner, so no second persistence seam
// is needed.
func (d *PeerDelivery) WithOperationStore(store *state.Store) *PeerDelivery {
	d.ops = store
	return d
}

// ----------------------------------------------------------------------------
// Result/param types (delivery-owned, per the authoritative spec).
// ----------------------------------------------------------------------------

// NotifyResult is the explicit fire-and-forget notify outcome
// (NotifyDeliveryResult). Reason is honest about what was proven:
// transport_delivered (live WS write), broker_accepted (ACP prompt dispatched,
// no runtime receipt), queued_delivery (no live transport → durable queue).
type NotifyResult struct {
	Status                string // "sent" | "queued"
	DeliveryState         string // "delivered" | "queued"
	Reason                string // transport_delivered | broker_accepted | queued_delivery
	FromPeerID            *proto.PeerID
	FromPeerName          proto.DisplayName
	ToPeerID              proto.PeerID
	ToPeerName            proto.DisplayName
	HookDelivery          map[string]any
	DeliveryID            string
	Transport             string // "ws" | "acp"
	RepowireSessionID     *string
	FromRepowireSessionID *string
	ToRepowireSessionID   *string
}

// Delivered reports whether the message reached a live transport.
func (r NotifyResult) Delivered() bool { return r.DeliveryState == "delivered" }

// Queued reports whether the message was held in the durable queue.
func (r NotifyResult) Queued() bool { return r.DeliveryState == "queued" }

// AskResult records which transport delivered and the optional hook receipt, so
// callers write truthful delivery-trace stages (pane_injected vs
// injection_failed).
type AskResult struct {
	Transport    string // "ws" | "acp"
	HookDelivery map[string]any
}

// NotifyParams are the inputs to Notify. FromPeer/ToPeer are peer_id or
// display_name as supplied by the caller (CheckAccess resolves them).
type NotifyParams struct {
	FromPeer     string
	ToPeer       string
	Text         string
	BypassCircle bool
	Circle       *string
	Attachments  []map[string]any
	DeliveryID   string
}

// DeliverAskParams are the inputs to DeliverAsk (the ask is ALREADY registered
// in the AskTracker by the caller).
type DeliverAskParams struct {
	FromPeer      string
	ToPeer        string
	Text          string
	CorrelationID string
	ReplyTo       *string
	BypassCircle  bool
	Circle        *string
	Attachments   []map[string]any
	Question      map[string]any
	// OnACPComplete is the ACP reply callback; nil → default (notify the asker /
	// stash on offline). Relevant only when ACPRoute returns a decision.
	OnACPComplete func(ctx context.Context, cid string, reply, errMsg *string)
}

// ----------------------------------------------------------------------------
// Notify
// ----------------------------------------------------------------------------

// Notify resolves+authorizes the target via reg.CheckAccess, seed-gates a
// pending_first_turn WS target, then sends via the chosen transport. On a
// no-live-transport TransportError it marks the peer offline and enqueues to the
// durable queue; if the queue is disabled it returns the error (fail loud →
// 503). deliveryID "" lets the transport mint one.
func (d *PeerDelivery) Notify(ctx context.Context, params NotifyParams) (NotifyResult, error) {
	from, target, err := d.reg.CheckAccess(ctx, params.FromPeer, params.ToPeer, params.BypassCircle, params.Circle)
	if err != nil {
		return NotifyResult{}, err
	}

	fromName := proto.DisplayName(params.FromPeer)
	var fromID *proto.PeerID
	if from != nil {
		fromName = from.DisplayName
		id := from.PeerID
		fromID = &id
	}

	d.gateOnSeedSettled(ctx, target)
	params.Text = addOrchestratorRecall(params.Text, string(fromName), peerIDString(fromID), target, d.recall)
	fromSessionID := d.sessionIDForPeer(ctx, fromID)
	toSessionID := d.sessionIDForPeer(ctx, &target.PeerID)

	if decision, ok := d.transport.ACPRoute(target); ok && decision != nil {
		if err := decision.Prompt(params.Text, func(_ ACPPromptResult, err error) {
			if err != nil {
				log.Printf("delivery: ACP notify to %s failed: %v", target.PeerID, err)
			}
		}); err != nil {
			return NotifyResult{}, err
		}
		return NotifyResult{
			Status:                "sent",
			DeliveryState:         "delivered",
			Reason:                "broker_accepted",
			FromPeerID:            fromID,
			FromPeerName:          fromName,
			ToPeerID:              target.PeerID,
			ToPeerName:            target.DisplayName,
			DeliveryID:            params.DeliveryID,
			Transport:             "acp",
			RepowireSessionID:     toSessionID,
			FromRepowireSessionID: fromSessionID,
			ToRepowireSessionID:   toSessionID,
		}, nil
	}

	hookDelivery, err := d.router.SendNotification(
		ctx, fromName, target.PeerID, target.DisplayName,
		proto.DisplayName(params.ToPeer), params.Text, params.Attachments, params.DeliveryID,
	)
	if err != nil {
		if errors.Is(err, ErrNotConnected) || errors.Is(err, ErrTransportUnavailable) {
			return d.queueNotify(ctx, params, fromID, fromName, target, err)
		}
		return NotifyResult{}, err
	}

	return NotifyResult{
		Status:                "sent",
		DeliveryState:         "delivered",
		Reason:                "transport_delivered",
		FromPeerID:            fromID,
		FromPeerName:          fromName,
		ToPeerID:              target.PeerID,
		ToPeerName:            target.DisplayName,
		HookDelivery:          hookDelivery,
		DeliveryID:            params.DeliveryID,
		Transport:             "ws",
		RepowireSessionID:     toSessionID,
		FromRepowireSessionID: fromSessionID,
		ToRepowireSessionID:   toSessionID,
	}, nil
}

// queueNotify is the no-live-transport fallback: mark the peer offline, enqueue
// to the durable queue, and return a queued result. If the queue is disabled
// (store nil, or EnqueueDelivery returns nil because cap/ttl <= 0) the original
// transport error propagates — fail loud, never silently drop.
func (d *PeerDelivery) queueNotify(
	ctx context.Context,
	params NotifyParams,
	fromID *proto.PeerID,
	fromName proto.DisplayName,
	target *proto.Peer,
	transportErr error,
) (NotifyResult, error) {
	d.markTransportUnreachable(ctx, target, "notify", transportErr)

	if d.store == nil {
		return NotifyResult{}, transportErr
	}
	var fromIDStr *string
	if fromID != nil {
		s := string(*fromID)
		fromIDStr = &s
	}
	attachments := params.Attachments
	if attachments == nil {
		attachments = []map[string]any{}
	}
	queued, err := d.store.EnqueueDelivery(ctx, state.QueuedDelivery{
		PeerID:            string(target.PeerID),
		RepowireSessionID: d.sessionIDForPeer(ctx, &target.PeerID),
		Kind:              state.DeliveryNotify,
		FromPeerID:        fromIDStr,
		FromPeerName:      string(fromName),
		ToPeerName:        string(target.DisplayName),
		Text:              params.Text,
		Attachments:       attachments,
	}, d.queueTTLSeconds, d.queueMax, time.Time{})
	if err != nil {
		return NotifyResult{}, err
	}
	if queued == nil {
		// Queue disabled (cap/ttl <= 0): nothing durable was written. Fail loud.
		return NotifyResult{}, transportErr
	}

	d.reg.AddEvent(ctx, "notification", map[string]any{
		"from":              string(fromName),
		"to":                string(target.DisplayName),
		"text":              params.Text,
		"from_peer_id":      fromIDStr,
		"to_peer_id":        string(target.PeerID),
		"delivery_status":   "queued",
		"delivery_state":    "queued",
		"queue_delivery_id": queued.DeliveryID,
		"attachments":       attachments,
	})

	return NotifyResult{
		Status:                "queued",
		DeliveryState:         "queued",
		Reason:                "queued_delivery",
		FromPeerID:            fromID,
		FromPeerName:          fromName,
		ToPeerID:              target.PeerID,
		ToPeerName:            target.DisplayName,
		DeliveryID:            params.DeliveryID,
		RepowireSessionID:     d.sessionIDForPeer(ctx, &target.PeerID),
		FromRepowireSessionID: d.sessionIDForPeer(ctx, fromID),
		ToRepowireSessionID:   d.sessionIDForPeer(ctx, &target.PeerID),
	}, nil
}

func (d *PeerDelivery) sessionIDForPeer(ctx context.Context, peerID *proto.PeerID) *string {
	if d.ops == nil || peerID == nil || *peerID == "" {
		return nil
	}
	bindings, err := d.ops.ListBindingsByPeer(ctx, string(*peerID))
	if err != nil || len(bindings) == 0 {
		return nil
	}
	id := bindings[0].RepowireSessionID
	return &id
}

// ----------------------------------------------------------------------------
// DeliverAsk
// ----------------------------------------------------------------------------

// DeliverAsk delivers an ALREADY-REGISTERED ask (caller registers in the
// AskTracker first). CheckAccess → seed-gate → transport choice. A
// *DeliveryInjectionError propagates unchanged (the route records
// injection_failed + 503; the peer is NOT marked unreachable — the socket is
// alive). A genuine TransportError (no live socket) marks the peer offline and
// propagates.
func (d *PeerDelivery) DeliverAsk(ctx context.Context, params DeliverAskParams) (AskResult, error) {
	from, target, err := d.reg.CheckAccess(ctx, params.FromPeer, params.ToPeer, params.BypassCircle, params.Circle)
	if err != nil {
		return AskResult{}, err
	}

	fromName := proto.DisplayName(params.FromPeer)
	if from != nil {
		fromName = from.DisplayName
	}

	d.gateOnSeedSettled(ctx, target)
	params.Text = addOrchestratorRecall(params.Text, string(fromName), stringValuePeer(from), target, d.recall)

	if decision, ok := d.transport.ACPRoute(target); ok && decision != nil {
		opID, err := d.recordACPAsk(ctx, params.CorrelationID, params.FromPeer, from, target)
		if err != nil {
			return AskResult{}, err
		}
		callback := params.OnACPComplete
		if callback == nil {
			callback = d.completeACPAsk
		}
		err = decision.Prompt(params.Text, func(result ACPPromptResult, promptErr error) {
			var reply, errText *string
			if promptErr != nil {
				text := promptErr.Error()
				errText = &text
			} else {
				text := result.Text
				if text == "" {
					text = fmt.Sprintf("[acp stop_reason=%s, no text]", result.StopReason)
				}
				reply = &text
			}
			callback(context.Background(), params.CorrelationID, reply, errText)
			d.settleACPAsk(context.Background(), opID, errText)
		})
		if err != nil {
			detail := err.Error()
			d.settleACPAsk(ctx, opID, &detail)
			return AskResult{}, err
		}
		return AskResult{Transport: "acp"}, nil
	}

	hookDelivery, err := d.router.SendAsk(
		ctx, fromName, target.PeerID, target.DisplayName, proto.DisplayName(params.ToPeer),
		params.CorrelationID, params.Text, params.ReplyTo, params.Question, params.Attachments,
	)
	if err != nil {
		// Injection failure at a live pane: do NOT mark unreachable (fail loud,
		// propagate so the route records injection_failed + 503).
		if _, ok := AsDeliveryInjection(err); ok {
			return AskResult{}, err
		}
		if errors.Is(err, ErrNotConnected) || errors.Is(err, ErrTransportUnavailable) {
			d.markTransportUnreachable(ctx, target, "ask", err)
		}
		return AskResult{}, err
	}

	return AskResult{Transport: "ws", HookDelivery: hookDelivery}, nil
}

func peerIDString(value *proto.PeerID) string {
	if value == nil {
		return ""
	}
	return string(*value)
}
func stringValuePeer(value *proto.Peer) string {
	if value == nil {
		return ""
	}
	return string(value.PeerID)
}

func (d *PeerDelivery) completeACPAsk(ctx context.Context, cid string, reply, errText *string) {
	if d.asks == nil {
		return
	}
	ask, ok := d.asks.Get(cid)
	if !ok || ask.Closed {
		return
	}
	isError := errText != nil
	body := ""
	if reply != nil {
		body = *reply
	}
	if isError {
		body = "ACP error: " + *errText
	}
	framed := fmt.Sprintf("[ack #%s from @%s] %s", cid, ask.ToPeerName, body)
	result, err := d.Notify(ctx, NotifyParams{FromPeer: string(ask.ToPeerID), ToPeer: string(ask.FromPeerID), Text: framed, BypassCircle: true})
	if err == nil && result.Queued() && !isError {
		if reply != nil {
			d.asks.CaptureReply(ctx, cid, *reply, nil)
		}
		_, _ = d.asks.Close(ctx, cid, "ack_with_msg")
		return
	}
	if err != nil || !result.Delivered() {
		if isError {
			_, _ = d.asks.Close(ctx, cid, "send_failed")
			return
		}
		var identity *AskerIdentity
		if asker, found := d.reg.GetPeer(ask.FromPeerID); found && asker.Machine != "" && asker.Machine != "unknown" && asker.Path != "" {
			identity = &AskerIdentity{DisplayName: asker.DisplayName, Circle: asker.Circle, Backend: asker.Backend, Path: asker.Path, Machine: asker.Machine}
		}
		d.asks.SetPendingReply(ctx, cid, framed, identity, false)
		return
	}
	if !isError && reply != nil {
		d.asks.CaptureReply(ctx, cid, *reply, nil)
	}
	reason := "ack_with_msg"
	if isError {
		reason = "send_failed"
	}
	_, _ = d.asks.Close(ctx, cid, reason)
}

// OpenScheduledAsk registers and delivers a scheduled ask, rolling back the
// AskTracker entry on send failure. Job dispatch passes replyDelivery="pull":
// the @jobs sender has no transport, so the executor's ack reply is retained on
// the ask instead of attempting a notify back to a peer that cannot receive it.
func (d *PeerDelivery) OpenScheduledAsk(ctx context.Context, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error) {
	if d.asks == nil {
		return "", errors.New("delivery: AskTracker is required to open scheduled asks")
	}
	cid, err := d.asks.Register(ctx, RegisterAskParams{
		FromPeerID:    proto.PeerID(fromPeer),
		FromPeerName:  proto.DisplayName(fromPeer),
		ToPeerID:      proto.PeerID(toPeer),
		ToPeerName:    proto.DisplayName(toPeer),
		Text:          text,
		ReplyDelivery: replyDelivery,
	})
	if err != nil {
		return "", err
	}
	if _, err := d.DeliverAsk(ctx, DeliverAskParams{
		FromPeer:      fromPeer,
		ToPeer:        toPeer,
		Text:          text,
		CorrelationID: cid,
		Circle:        circle,
	}); err != nil {
		_, _ = d.asks.Close(ctx, cid, "send_failed")
		return "", err
	}
	return cid, nil
}

// ----------------------------------------------------------------------------
// Broadcast
// ----------------------------------------------------------------------------

// Broadcast fans out best-effort to all eligible connected peers (circle-gated
// unless the sender bypasses), deferring pending_first_turn WS peers behind the
// seed gate and (when ACPRoute is live) routing ACP peers separately. Returns
// the delivered display-names and per-recipient failures. Mirrors
// PeerDeliveryService.broadcast.
func (d *PeerDelivery) Broadcast(ctx context.Context, fromPeer, text string, exclude []string, bypassCircle bool) (sent []proto.DisplayName, failed []BroadcastFailure) {
	excludeNames := map[string]struct{}{fromPeer: {}}
	for _, n := range exclude {
		excludeNames[n] = struct{}{}
	}

	excludeIDs := map[proto.PeerID]struct{}{}
	// Resolve the sender to learn its circle / bypass status. bypass_circle here
	// so an unknown/cross-circle sender lookup never errors the broadcast.
	fromObj, _, _ := d.reg.CheckAccess(ctx, fromPeer, fromPeer, true, nil)

	peers := d.reg.GetAllPeers()
	idToName := map[proto.PeerID]proto.DisplayName{}
	for _, p := range peers {
		idToName[p.PeerID] = p.DisplayName
		if _, ex := excludeNames[string(p.DisplayName)]; ex {
			excludeIDs[p.PeerID] = struct{}{}
		}
		if _, ex := excludeNames[string(p.PeerID)]; ex {
			excludeIDs[p.PeerID] = struct{}{}
		}
	}

	senderBypasses := fromObj != nil && (bypassCircle || fromObj.Role.BypassesCircles())
	if fromObj != nil && !senderBypasses {
		for _, p := range peers {
			if p.Circle != fromObj.Circle && !p.Role.BypassesCircles() {
				excludeIDs[p.PeerID] = struct{}{}
			}
		}
	}

	var fromIDPtr *string
	if fromObj != nil {
		s := string(fromObj.PeerID)
		fromIDPtr = &s
	}
	d.reg.AddEvent(ctx, "broadcast", map[string]any{
		"from":         fromPeer,
		"text":         text,
		"exclude":      exclude,
		"from_peer_id": fromIDPtr,
	})

	// ACP-routed recipients have no WS session, so route them separately before
	// the WebSocket fanout. Broadcast replies are intentionally discarded.
	for _, p := range peers {
		if _, ex := excludeIDs[p.PeerID]; ex {
			continue
		}
		if p.Status == proto.StatusOffline {
			continue
		}
		if decision, ok := d.transport.ACPRoute(p); ok && decision != nil {
			excludeIDs[p.PeerID] = struct{}{}
			if err := decision.Prompt(text, func(_ ACPPromptResult, err error) {
				if err != nil {
					log.Printf("delivery: ACP broadcast to %s failed: %v", p.PeerID, err)
				}
			}); err != nil {
				failed = append(failed, BroadcastFailure{PeerID: p.PeerID, Error: err.Error()})
			} else {
				sent = append(sent, p.DisplayName)
			}
		}
	}

	// WS recipients still seeding (pending_first_turn) would have the broadcast
	// interleaved with their in-flight spawn seed: deliver each via a background
	// goroutine that awaits the seed gate first. One pending peer never blocks
	// the rest of the fanout.
	var deferredNames []proto.DisplayName
	for _, p := range peers {
		if _, ex := excludeIDs[p.PeerID]; ex {
			continue
		}
		if p.Status == proto.StatusOffline {
			continue
		}
		if p.TurnState != proto.TurnPendingFirstTurn {
			continue
		}
		excludeIDs[p.PeerID] = struct{}{}
		d.deferBroadcastUntilSeedSettled(fromPeer, text, p.PeerID)
		deferredNames = append(deferredNames, p.DisplayName)
	}

	sentIDs, failures := d.router.Broadcast(ctx, proto.DisplayName(fromPeer), text, excludeIDs)
	for _, id := range sentIDs {
		if name, ok := idToName[id]; ok {
			sent = append(sent, name)
		}
	}
	sent = append(sent, deferredNames...)
	return sent, failures
}

// ----------------------------------------------------------------------------
// Seed gate + transport-unreachable helpers
// ----------------------------------------------------------------------------

// gateOnSeedSettled holds a WS pane injection while target's spawn seed is in
// flight (target is pending_first_turn). Only WS-routed targets inject into a
// pane — ACP-routed targets prompt the broker (which serializes turns itself) —
// so this is a no-op for ACP. Bounded wait; proceeds anyway on timeout rather
// than re-queueing (there is no flush trigger to re-arm a live delivery).
func (d *PeerDelivery) gateOnSeedSettled(ctx context.Context, target *proto.Peer) {
	if target == nil || target.TurnState != proto.TurnPendingFirstTurn {
		return
	}
	if decision, ok := d.transport.ACPRoute(target); ok && decision != nil {
		return
	}
	d.awaitSeedSettled(ctx, target.PeerID)
}

// awaitSeedSettled polls the registry until the peer leaves pending_first_turn,
// the peer vanishes, the bounded deadline elapses, or ctx is cancelled. Mirrors
// seed_gate.await_seed_settled.
func (d *PeerDelivery) awaitSeedSettled(ctx context.Context, id proto.PeerID) {
	deadline := time.Now().Add(seedSettleWait)
	for {
		p, ok := d.reg.GetPeer(id)
		if !ok || p.TurnState != proto.TurnPendingFirstTurn {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("delivery: %s proceeding while still pending_first_turn (seed not settled within %s)", id, seedSettleWait)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-d.closeCh:
			return
		case <-time.After(seedSettlePoll):
		}
	}
}

// deferBroadcastUntilSeedSettled schedules a single broadcast to a still-seeding
// WS peer in a background goroutine, tracked so Close can join it. The peer is
// already connected, so the queued-delivery store (which only flushes on ws
// connect) would strand the message — instead we wait out the seed gate and
// send directly. One pending peer must never block the rest of the broadcast
// fanout. No-op after Close (mirrors peer.Registry.spawnTracked's gate).
func (d *PeerDelivery) deferBroadcastUntilSeedSettled(fromPeer, text string, id proto.PeerID) {
	d.closeMu.Lock()
	if d.closed {
		d.closeMu.Unlock()
		return
	}
	d.wg.Add(1)
	d.closeMu.Unlock()
	go func() {
		defer d.wg.Done()
		ctx := context.Background()
		d.awaitSeedSettled(ctx, id)
		if err := d.router.BroadcastToSession(ctx, proto.DisplayName(fromPeer), text, id); err != nil {
			log.Printf("delivery: deferred broadcast to %s failed after seed gate: %v", id, err)
		}
	}()
}

// Close unblocks any goroutine parked in awaitSeedSettled's seed-gate poll
// (up to seedSettleWait=25s) and joins them. Call before the registry/store
// shut down — a deferred broadcast that outlives them would read/write closed
// state.
func (d *PeerDelivery) Close() {
	d.closeMu.Lock()
	if !d.closed {
		d.closed = true
		close(d.closeCh)
	}
	d.closeMu.Unlock()
	d.wg.Wait()
}

// markTransportUnreachable drives a transport-owned peer offline after a genuine
// TransportError. Pane-backed runtimes can outlive a disconnected sidecar, so
// lazy repair owns their runtime-evidence check before changing lifecycle state.
func (d *PeerDelivery) markTransportUnreachable(ctx context.Context, target *proto.Peer, operation string, transportErr error) {
	if target == nil {
		return
	}
	if target.PaneID != nil && *target.PaneID != "" {
		return
	}
	if target.Metadata != nil {
		if v, ok := target.Metadata["repowire_cli_fallback"].(bool); ok && v {
			return
		}
	}
	if _, err := d.reg.MarkOffline(ctx, target.PeerID, false); err != nil {
		log.Printf("delivery: mark %s offline after %s transport failure failed: %v", target.PeerID, operation, err)
		return
	}
	log.Printf("delivery: marked peer %s offline after %s transport failure: %v", target.PeerID, operation, transportErr)
}
