package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

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
	// inbox, when wired, owns delivery to the human peer: a message to the
	// human is stored there instead of written to a socket.
	inbox *Inbox
	// hosted, when wired, owns delivery to managed agents: a message becomes a
	// durable submission to the hosted runtime.
	hosted *Hosted

	// closeMu/closed/wg/closeCh track the deferBroadcastUntilSeedSettled
	// goroutines so Close can join them without waiting out a 25s seed-gate
	// poll. See Close and awaitSeedSettled.
	closeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	closeCh chan struct{}
}

// WithInbox routes delivery to the human peer through inbox.
func (d *PeerDelivery) WithInbox(inbox *Inbox) *PeerDelivery {
	d.inbox = inbox
	if d.router != nil && inbox != nil {
		d.router.human = inbox
	}
	return d
}

// WithHosted routes delivery to hosted peers through hosted and closes their
// asks with the runtime's answer.
func (d *PeerDelivery) WithHosted(hosted *Hosted) *PeerDelivery {
	d.hosted = hosted
	if hosted != nil {
		hosted.OnSettled("reply", d.completeHosted)
	}
	return d
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
// transport_delivered (live WS write), broker_accepted (ACP prompt dispatched),
// recipient_busy, or queued_delivery (no live transport → durable queue).
type NotifyResult struct {
	Status        string // "sent" | "queued"
	DeliveryState string // "delivered" | "queued"
	Reason        string // transport_delivered | broker_accepted | recipient_busy | queued_delivery
	FromPeerID    *proto.PeerID
	FromPeerName  proto.DisplayName
	ToPeerID      proto.PeerID
	ToPeerName    proto.DisplayName
	HookDelivery  map[string]any
	DeliveryID    string
	Transport     string // "ws" | "acp" | "inbox" | "hosted"
	// Inbox is set when the recipient is the human peer: the stored item and
	// the surfaces a push was started on.
	Inbox                 *InboxReceipt
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
	Transport    string // "ws" | "acp" | "deferred" | "inbox" | "hosted"
	HookDelivery map[string]any
	Inbox        *InboxReceipt
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
	// SkipSeedGate marks the spawn seed itself: the first prompt that settles a
	// pending_first_turn peer must not wait behind the gate meant to hold other
	// traffic until that seed has landed.
	SkipSeedGate bool
	// CorrelationID marks this notify as a reply on an ask thread. Only the
	// human inbox uses it (to thread the item); it never reaches a WS frame and
	// never opens or closes an ask.
	CorrelationID string
	// ThreadText is the reply body without the "[ack #cid from @x]" frame, for
	// a recipient that threads natively. Empty means use Text.
	ThreadText string
	// NoFallbackQueue makes a failed send an error instead of a queued
	// retry, for callers that promise at most one dispatch (cross-machine
	// notifies). A busy recipient is still queued, and reported Queued.
	NoFallbackQueue bool
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
	// The registry gates this too; repeated here so a fake registry in tests
	// and any future registry seam cannot route inbound work past the verdict.
	if err := proto.RequireAddressable(from, target); err != nil {
		return NotifyResult{}, err
	}

	fromName := proto.DisplayName(params.FromPeer)
	var fromID *proto.PeerID
	if from != nil {
		fromName = from.DisplayName
		id := from.PeerID
		fromID = &id
	}
	if d.inbox.Owns(target) {
		return d.notifyInbox(ctx, params, fromID, fromName, target)
	}
	if d.hosted.Owns(target) {
		receipt, err := d.hosted.Deliver(ctx, "notify", "", from, string(fromName), target, params.Text)
		if err != nil {
			return NotifyResult{}, err
		}
		return NotifyResult{
			Status: "sent", DeliveryState: "delivered", Reason: "runtime_accepted",
			FromPeerID: fromID, FromPeerName: fromName, ToPeerID: target.PeerID, ToPeerName: target.DisplayName,
			HookDelivery: receipt, DeliveryID: params.DeliveryID, Transport: "hosted",
		}, nil
	}

	if !params.SkipSeedGate {
		d.gateOnSeedSettled(ctx, target)
	}
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
	if wsRecipientBusy(target) {
		return d.queueNotify(ctx, params, fromID, fromName, target,
			fmt.Errorf("peer %s is busy and durable queue is unavailable", target.DisplayName), true)
	}

	hookDelivery, err := d.router.SendNotification(
		ctx, SenderOf(from, params.FromPeer), target.PeerID, target.DisplayName,
		proto.DisplayName(params.ToPeer), params.Text, params.Attachments, params.DeliveryID,
	)
	if err != nil {
		if !params.NoFallbackQueue && (errors.Is(err, ErrNotConnected) || errors.Is(err, ErrTransportUnavailable)) {
			return d.queueNotify(ctx, params, fromID, fromName, target, err, false)
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

// notifyInbox stores a notify (or a threaded reply) in the human inbox. The
// item is durable once stored, so the result is delivered whether or not a
// surface is live; pushes to surfaces continue in the background.
func (d *PeerDelivery) notifyInbox(ctx context.Context, params NotifyParams, fromID *proto.PeerID, fromName proto.DisplayName, target *proto.Peer) (NotifyResult, error) {
	deliveryID := params.DeliveryID
	if deliveryID == "" {
		deliveryID = "notif-delivery-" + uuid.NewString()[:8]
	}
	kind, traceID, traceKind, text := InboxNotify, deliveryID, "notify", params.Text
	if params.CorrelationID != "" {
		// A reply on an ask thread joins that ask's trace.
		kind, traceID, traceKind = InboxAnswer, params.CorrelationID, "ask"
		if params.ThreadText != "" {
			text = params.ThreadText
		}
	}
	receipt, err := d.inbox.Store(ctx, InboxStoreParams{
		Kind: kind, TraceID: traceID, TraceKind: traceKind,
		FromPeerID: peerIDString(fromID), FromName: string(fromName),
		CorrelationID: params.CorrelationID, Text: text, Attachments: params.Attachments,
	})
	if err != nil {
		return NotifyResult{}, err
	}
	return NotifyResult{
		Status:        "sent",
		DeliveryState: "delivered",
		Reason:        "inbox_stored",
		FromPeerID:    fromID,
		FromPeerName:  fromName,
		ToPeerID:      target.PeerID,
		ToPeerName:    target.DisplayName,
		HookDelivery:  inboxHookDelivery(receipt),
		DeliveryID:    deliveryID,
		Transport:     "inbox",
		Inbox:         &receipt,
	}, nil
}

// inboxHookDelivery is the receipt in the shape callers already record:
// recordDeliveryOutcome reads status, and the inbox has written its own stage.
func inboxHookDelivery(receipt InboxReceipt) map[string]any {
	surfaces := receipt.Surfaces
	if surfaces == nil {
		surfaces = []string{}
	}
	return map[string]any{"status": "inbox_stored", "item_id": receipt.ItemID, "surfaces": surfaces}
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
	recipientBusy bool,
) (NotifyResult, error) {
	if !recipientBusy {
		d.markTransportUnreachable(ctx, target, "notify", transportErr)
	}

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

	reason := "queued_delivery"
	if recipientBusy {
		reason = "recipient_busy"
	}
	return NotifyResult{
		Status:                "queued",
		DeliveryState:         "queued",
		Reason:                reason,
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
	if err := proto.RequireAddressable(from, target); err != nil {
		return AskResult{}, err
	}

	fromName := proto.DisplayName(params.FromPeer)
	if from != nil {
		fromName = from.DisplayName
	}
	if d.inbox.Owns(target) {
		receipt, err := d.inbox.Store(ctx, InboxStoreParams{
			Kind: InboxAsk, TraceID: params.CorrelationID, TraceKind: "ask",
			FromPeerID: stringValuePeer(from), FromName: string(fromName),
			CorrelationID: params.CorrelationID, Text: params.Text,
			Attachments: params.Attachments, Question: params.Question,
		})
		if err != nil {
			return AskResult{}, err
		}
		return AskResult{Transport: "inbox", HookDelivery: inboxHookDelivery(receipt), Inbox: &receipt}, nil
	}
	if d.hosted.Owns(target) {
		receipt, err := d.hosted.Deliver(ctx, "ask", params.CorrelationID, from, string(fromName), target, params.Text)
		if err != nil {
			return AskResult{}, err
		}
		return AskResult{Transport: "hosted", HookDelivery: receipt}, nil
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
	// The open AskTracker row is the queue: Stop surfaces it after the active
	// turn, avoiding tmux input that interrupts Codex and similar runtimes.
	if wsRecipientBusy(target) {
		return AskResult{Transport: "deferred"}, nil
	}

	hookDelivery, err := d.router.SendAsk(
		ctx, SenderOf(from, params.FromPeer), target.PeerID, target.DisplayName, proto.DisplayName(params.ToPeer),
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

func wsRecipientBusy(peer *proto.Peer) bool {
	if proto.HasCapability(peer.Metadata, proto.CapRuntimeInbox) || proto.HasCapability(peer.Metadata, proto.CapThreadSteering) {
		return false
	}
	return peer.Status == proto.StatusBusy || peer.TurnState == proto.TurnWorking
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
	_ = d.completeRuntimeAsk(ctx, cid, reply, errText, "ACP error: ")
}

// completeHosted closes an ask with the hosted runtime's answer, or with its
// failure. An ask the tracker no longer holds (the daemon restarted while the
// runtime worked) still reaches its asker as a framed reply. It fails when
// the reply was neither delivered nor durably queued, so the report is
// resent.
func (d *PeerDelivery) completeHosted(ctx context.Context, delivery HostedDelivery, result HostedResult) error {
	var reply, errText *string
	if result.Status != "done" {
		text := "no answer (" + result.Reason + ")"
		errText = &text
	} else {
		reply = &result.Text
	}
	if delivery.Kind != "ask" {
		if errText != nil {
			log.Printf("delivery: hosted %s to %s failed: %s", delivery.Kind, delivery.ToPeerID, *errText)
		}
		return nil
	}
	if d.asks == nil {
		return nil
	}
	// An ask still tracked closes here; one already closed (a job's completion
	// closed it first) needs nothing more.
	if _, ok := d.asks.Get(delivery.CorrelationID); ok {
		return d.completeRuntimeAsk(ctx, delivery.CorrelationID, reply, errText, "Hosted error: ")
	}
	body := ""
	if reply != nil {
		body = *reply
	} else {
		body = "Hosted error: " + *errText
	}
	framed := fmt.Sprintf("[ack #%s from @%s] %s", delivery.CorrelationID, delivery.ToPeerName, body)
	if _, err := d.Notify(ctx, NotifyParams{FromPeer: delivery.ToPeerID, ToPeer: delivery.FromPeerID, Text: framed,
		BypassCircle: true, CorrelationID: delivery.CorrelationID, ThreadText: body}); err == nil {
		return nil
	}
	// The asker has not re-registered since the restart: hold the reply in its
	// durable queue, which drains when it reconnects.
	if d.store == nil || delivery.FromPeerID == "" {
		log.Printf("delivery: hosted reply %s to %s undeliverable: no durable queue", delivery.CorrelationID, delivery.FromPeerName)
		return nil
	}
	queued, err := d.store.EnqueueDelivery(ctx, state.QueuedDelivery{
		PeerID: delivery.FromPeerID, Kind: state.DeliveryNotify, FromPeerID: stringPtr(delivery.ToPeerID),
		FromPeerName: delivery.ToPeerName, ToPeerName: delivery.FromPeerName, Text: framed,
	}, d.queueTTLSeconds, d.queueMax, time.Time{})
	if err != nil {
		return fmt.Errorf("queue hosted reply %s for %s: %w", delivery.CorrelationID, delivery.FromPeerID, err)
	}
	if queued == nil {
		log.Printf("delivery: hosted reply %s to %s undeliverable: the durable queue is disabled", delivery.CorrelationID, delivery.FromPeerID)
	}
	return nil
}

// completeRuntimeAsk closes an ask a runtime answered on the daemon's side
// (ACP, hosted): it notifies the asker with the framed reply, or stashes it
// when the asker cannot take it now. The stash lives only in memory, so that
// case returns an error; a caller that can retry (hosted) does.
func (d *PeerDelivery) completeRuntimeAsk(ctx context.Context, cid string, reply, errText *string, errPrefix string) error {
	if d.asks == nil {
		return nil
	}
	ask, ok := d.asks.Get(cid)
	if !ok || ask.Closed {
		return nil
	}
	isError := errText != nil
	body := ""
	if reply != nil {
		body = *reply
	}
	if isError {
		body = errPrefix + *errText
	}
	// A pull asker (relay MCP, jobs) retrieves the reply from the ask itself,
	// exactly as an ack would leave it; it has no socket to push to.
	if ask.ReplyDelivery == "pull" {
		reason := "ack_with_msg"
		if isError {
			reason = "send_failed"
		}
		if _, resolved := d.asks.Resolve(ctx, cid, reason, &body, nil); !resolved {
			// A concurrent close won, or the close guard (a remote ask's
			// durable settlement) refused it. Only the latter keeps the reply
			// unrecorded, so the caller must retry.
			if still, ok := d.asks.Get(cid); ok && !still.Closed {
				return fmt.Errorf("reply %s not recorded: the ask is still open", cid)
			}
		}
		return nil
	}
	framed := fmt.Sprintf("[ack #%s from @%s] %s", cid, ask.ToPeerName, body)
	result, err := d.Notify(ctx, NotifyParams{FromPeer: string(ask.ToPeerID), ToPeer: string(ask.FromPeerID), Text: framed, BypassCircle: true, CorrelationID: cid, ThreadText: body})
	if err == nil && result.Queued() && !isError {
		if reply != nil {
			d.asks.CaptureReply(ctx, cid, *reply, nil)
		}
		_, _ = d.asks.Close(ctx, cid, "ack_with_msg")
		return nil
	}
	if err != nil || !result.Delivered() {
		if isError {
			_, _ = d.asks.Close(ctx, cid, "send_failed")
			return nil
		}
		var identity *AskerIdentity
		if asker, found := d.reg.GetPeer(ask.FromPeerID); found && asker.Machine != "" && asker.Machine != "unknown" && asker.Path != "" {
			identity = &AskerIdentity{DisplayName: asker.DisplayName, Circle: asker.Circle, Backend: asker.Backend, Path: asker.Path, Machine: asker.Machine}
		}
		d.asks.SetPendingReply(ctx, cid, framed, identity, false)
		return fmt.Errorf("reply %s held only in memory for %s", cid, ask.FromPeerName)
	}
	if !isError && reply != nil {
		d.asks.CaptureReply(ctx, cid, *reply, nil)
	}
	reason := "ack_with_msg"
	if isError {
		reason = "send_failed"
	}
	_, _ = d.asks.Close(ctx, cid, reason)
	return nil
}

// OpenScheduledAsk registers and delivers a scheduled ask under cid (minted
// when empty), rolling back the AskTracker entry on send failure. Job dispatch passes replyDelivery="pull":
// the @jobs sender has no transport, so the executor's ack reply is retained on
// the ask instead of attempting a notify back to a peer that cannot receive it.
func (d *PeerDelivery) OpenScheduledAsk(ctx context.Context, cid, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error) {
	if d.asks == nil {
		return "", errors.New("delivery: AskTracker is required to open scheduled asks")
	}
	cid, err := d.asks.Register(ctx, RegisterAskParams{
		CorrelationID: cid,
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
	sender := SenderOf(fromObj, fromPeer)

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
	// A broadcast is inbound work for each recipient: peers that cannot take
	// direct input are skipped rather than counted as failures.
	for _, p := range peers {
		if proto.RequireAddressable(fromObj, p) != nil {
			excludeIDs[p.PeerID] = struct{}{}
		}
	}
	// With an inbox, the human and the surfaces that render it never take a
	// broadcast frame directly; the inbox decides (opt-in, default off).
	if d.inbox != nil {
		for _, p := range peers {
			if d.inbox.Owns(p) || proto.IsInboxSurface(p) {
				excludeIDs[p.PeerID] = struct{}{}
			}
		}
		if name, ok := d.broadcastToInbox(ctx, fromObj, fromPeer, text, excludeNames); ok {
			sent = append(sent, name)
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

	// ACP-routed and hosted recipients have no WS session, so route them
	// separately before the WebSocket fanout. Broadcast replies are
	// intentionally discarded.
	fromName := fromPeer
	if fromObj != nil {
		fromName = string(fromObj.DisplayName)
	}
	for _, p := range peers {
		if _, ex := excludeIDs[p.PeerID]; ex {
			continue
		}
		if p.Status == proto.StatusOffline {
			continue
		}
		if d.hosted.Owns(p) {
			excludeIDs[p.PeerID] = struct{}{}
			if _, err := d.hosted.Deliver(ctx, "broadcast", "", fromObj, fromName, p, text); err != nil {
				failed = append(failed, BroadcastFailure{PeerID: p.PeerID, Error: err.Error()})
			} else {
				sent = append(sent, p.DisplayName)
			}
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
		d.deferBroadcastUntilSeedSettled(sender, text, p.PeerID)
		deferredNames = append(deferredNames, p.DisplayName)
	}

	sentIDs, failures := d.router.Broadcast(ctx, sender, text, excludeIDs)
	for _, id := range sentIDs {
		if name, ok := idToName[id]; ok {
			sent = append(sent, name)
		}
	}
	sent = append(sent, deferredNames...)
	return sent, failures
}

// broadcastToInbox stores a broadcast for the human when human.broadcast is on
// and the human was neither the sender nor excluded. ok reports a stored item.
func (d *PeerDelivery) broadcastToInbox(ctx context.Context, from *proto.Peer, fromPeer, text string, exclude map[string]struct{}) (proto.DisplayName, bool) {
	human, name := d.inbox.Human()
	if human == "" || !d.inbox.cfg.Broadcast || (from != nil && from.PeerID == human) {
		return "", false
	}
	if _, skip := exclude[string(human)]; skip {
		return "", false
	}
	if _, skip := exclude[string(name)]; skip {
		return "", false
	}
	fromName := fromPeer
	if from != nil {
		fromName = string(from.DisplayName)
	}
	if _, err := d.inbox.Store(ctx, InboxStoreParams{
		Kind: InboxBroadcast, TraceKind: "broadcast",
		FromPeerID: stringValuePeer(from), FromName: fromName, Text: text,
	}); err != nil {
		log.Printf("delivery: broadcast to inbox failed: %v", err)
		return "", false
	}
	return name, true
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
func (d *PeerDelivery) deferBroadcastUntilSeedSettled(from Sender, text string, id proto.PeerID) {
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
		if err := d.router.BroadcastToSession(ctx, from, text, id); err != nil {
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
