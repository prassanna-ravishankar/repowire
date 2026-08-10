package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// ----------------------------------------------------------------------------
// Transport seam.
//
// *WebSocketTransport (transport.go) implements this narrower interface plus the
// full wire primitives. The router (and, one layer up, PeerDelivery) depend only
// on Transport so they stay testable. ACPRoute is the only ACP-aware method;
// transport choice remains above the wire-level router.
// ----------------------------------------------------------------------------

// Transport is the narrow seam the router/delivery layer routes against. The
// concrete *WebSocketTransport satisfies it; tests fake it.
type Transport interface {
	Send(ctx context.Context, id proto.PeerID, v any) error
	SendAndWaitDeliveryAck(ctx context.Context, id proto.PeerID, v any, timeout time.Duration) (map[string]any, error)
	IsConnected(id proto.PeerID) bool
	GetAllSessions() []proto.PeerID
	ConnectionPaneID(id proto.PeerID) (string, bool)
	Ping(ctx context.Context, id proto.PeerID, timeout time.Duration) (map[string]any, error)
	ACPRoute(target *proto.Peer) (decision *ACPRouteDecision, ok bool)
}

var _ Transport = (*WebSocketTransport)(nil)

// ACPRouteDecision is a validated ACP routing decision for one peer.
type ACPRouteDecision struct {
	PeerID  proto.PeerID
	Spec    ACPPeerSpec
	manager *ACPManager
}

// Prompt schedules a persistent ACP session/prompt turn. Completion runs on a
// broker goroutine; accepting the prompt is intentionally non-blocking.
func (d *ACPRouteDecision) Prompt(text string, complete func(ACPPromptResult, error)) error {
	if d == nil || d.manager == nil {
		return errors.New("ACP route has no manager")
	}
	return d.manager.Prompt(d.Spec, text, complete)
}

// ----------------------------------------------------------------------------
// DeliveryInjectionError — the typed fail-loud rejection.
//
// The ws-hook was reached but injection failed/was rejected at the pane. The
// connection is alive, so the peer must NOT be marked unreachable — the route
// records injection_failed and returns 503. Mirrors Python
// DeliveryInjectionError (a TransportError subclass carrying the hook receipt).
// ----------------------------------------------------------------------------

// DeliveryInjectionError is raised when a delivery_ack reports a terminal
// failed/rejected injection. It is distinct from ErrNotConnected (no live
// socket → no_connection trace), which is the plain-TransportError analogue.
type DeliveryInjectionError struct {
	Status       string         // "failed" | "rejected"
	Detail       string         // hook-supplied detail (falls back to Status)
	HookDelivery map[string]any // the terminal delivery_ack frame
}

func (e *DeliveryInjectionError) Error() string {
	return "ask injection " + e.Status + ": " + e.Detail
}

// AsDeliveryInjection unwraps err to a *DeliveryInjectionError if it is one.
func AsDeliveryInjection(err error) (*DeliveryInjectionError, bool) {
	var d *DeliveryInjectionError
	return d, errors.As(err, &d)
}

// ----------------------------------------------------------------------------
// MessageRouter — wire-level send + the blocking query future.
//
// The router speaks ONLY WS; transport CHOICE (ACP-before-WS) lives one layer up
// in PeerDelivery via Transport.ACPRoute. It is given the Transport seam (not the
// concrete *WebSocketTransport) so tests can fake it.
// ----------------------------------------------------------------------------

// MessageRouter turns a routing intent (send query/ask/notify to a PeerID) into
// a wire frame on the transport, and — for the blocking query path — waits on
// the QueryTracker future. Every routing function takes proto.PeerID as the
// target; passing a DisplayName is a compile error, which is the whole point.
type MessageRouter struct {
	transport Transport
	reg       *peer.Registry
}

// NewMessageRouter wires the router to the transport, tracker, and registry. The
// transport is taken by interface so the real *WebSocketTransport and a test
// fake are both accepted.
func NewMessageRouter(transport Transport, reg *peer.Registry) *MessageRouter {
	return &MessageRouter{transport: transport, reg: reg}
}

// SendNotification puts a fire-and-forget notify frame on the wire and waits up
// to deliveryAckTimeout for an optional hook receipt. Wire shape:
// {type:notify, delivery_id, from_peer, to_peer, text, attachments?}. Returns
// the hook_delivery map (nil when the hook didn't ack). deliveryID, when "", is
// minted as "notif-delivery-<hex8>". A missing ack is not an error (older hooks
// never ack). Mirrors MessageRouter.send_notification.
func (m *MessageRouter) SendNotification(
	ctx context.Context,
	from proto.DisplayName,
	to proto.PeerID,
	toName proto.DisplayName,
	intendedRecipientName proto.DisplayName,
	text string,
	attachments []map[string]any,
	deliveryID string,
) (hookDelivery map[string]any, err error) {
	if deliveryID == "" {
		deliveryID = "notif-delivery-" + uuid.NewString()[:8]
	}
	frame := map[string]any{
		"type":        string(proto.FrameNotify),
		"delivery_id": deliveryID,
		"from_peer":   string(from),
		"to_peer":     string(toName),
		"text":        text,
	}
	if len(attachments) > 0 {
		frame["attachments"] = attachments
	}
	m.logDeliveryTrace("Notify", from, intendedRecipientName, toName, to)
	return m.transport.SendAndWaitDeliveryAck(ctx, to, frame, deliveryAckTimeout)
}

// SendAsk puts a first-class ask frame on the wire and waits for the optional
// hook receipt. Wire shape: {type:ask, delivery_id, correlation_id, from_peer,
// to_peer, text(+close-hint), reply_to?, question?, attachments?}. A delivery_ack
// with status in {"failed","rejected"} is raised as a *DeliveryInjectionError
// (FAIL LOUD); any other ack is returned as hookDelivery. The daemon does NOT
// track pickup — open asks resurface via /asks/pending until acked. Mirrors
// MessageRouter.send_ask.
func (m *MessageRouter) SendAsk(
	ctx context.Context,
	from proto.DisplayName,
	to proto.PeerID,
	toName proto.DisplayName,
	intendedRecipientName proto.DisplayName,
	correlationID string,
	text string,
	replyTo *string,
	question map[string]any,
	attachments []map[string]any,
) (hookDelivery map[string]any, err error) {
	var hint string
	if question != nil {
		hint = fmt.Sprintf(`↳ answer("%s", option_id=...) or answer("%s", text="...")`, correlationID, correlationID)
	} else {
		hint = fmt.Sprintf(`↳ ack("%s") or ack("%s", "reply")`, correlationID, correlationID)
	}
	hintedText := strings.TrimRight(text, " \t\r\n") + "\n" + hint

	frame := map[string]any{
		"type":           string(proto.FrameAsk),
		"delivery_id":    "ask-delivery-" + uuid.NewString()[:8],
		"correlation_id": correlationID,
		"from_peer":      string(from),
		"to_peer":        string(toName),
		"text":           hintedText,
	}
	if len(attachments) > 0 {
		frame["attachments"] = attachments
	}
	if replyTo != nil {
		frame["reply_to"] = *replyTo
	}
	if question != nil {
		frame["question"] = question
	}
	m.logDeliveryTrace("Ask", from, intendedRecipientName, toName, to)

	ack, err := m.transport.SendAndWaitDeliveryAck(ctx, to, frame, deliveryAckTimeout)
	if err != nil {
		return nil, err
	}
	if ack != nil {
		if status, _ := ack["status"].(string); status == "failed" || status == "rejected" {
			detail, _ := ack["detail"].(string)
			if detail == "" {
				detail = status
			}
			// FAIL LOUD: the hook was reached but injection failed/was rejected.
			// The socket is alive — the caller must NOT mark the peer unreachable.
			return nil, &DeliveryInjectionError{Status: status, Detail: detail, HookDelivery: ack}
		}
	}
	return ack, nil
}

// BroadcastToSession sends one broadcast envelope to a single live WS session.
// Used for deferred broadcast to a peer that was pending_first_turn at fanout.
// Returns ErrNotConnected when the session has no live transport.
func (m *MessageRouter) BroadcastToSession(ctx context.Context, from proto.DisplayName, text string, to proto.PeerID) error {
	return m.transport.Send(ctx, to, map[string]any{
		"type":      string(proto.FrameBroadcast),
		"from_peer": string(from),
		"text":      text,
	})
}

// Broadcast fans out to every connected peer minus exclude (a set of PeerID),
// best-effort. Returns the sent peer_ids and per-recipient failures; one failure
// never aborts the fanout. Mirrors MessageRouter.broadcast.
func (m *MessageRouter) Broadcast(
	ctx context.Context,
	from proto.DisplayName,
	text string,
	exclude map[proto.PeerID]struct{},
) (sent []proto.PeerID, failed []BroadcastFailure) {
	frame := map[string]any{
		"type":      string(proto.FrameBroadcast),
		"from_peer": string(from),
		"text":      text,
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, id := range m.transport.GetAllSessions() {
		if _, skip := exclude[id]; skip {
			continue
		}
		wg.Add(1)
		go func(id proto.PeerID) {
			defer wg.Done()
			if err := m.transport.Send(ctx, id, frame); err != nil {
				mu.Lock()
				failed = append(failed, BroadcastFailure{PeerID: id, Error: err.Error()})
				mu.Unlock()
				return
			}
			mu.Lock()
			sent = append(sent, id)
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return sent, failed
}

// BroadcastFailure is one recipient whose transport raised.
type BroadcastFailure struct {
	PeerID proto.PeerID
	Error  string
}

// logDeliveryTrace emits the same structured delivery-trace log line the Python
// router logs before send (sender identity, intended recipient, resolved
// peer_id, frame.to_peer, actual delivered pane_id). Truthfulness only: it
// records what we resolved, never claims an injection that didn't happen.
func (m *MessageRouter) logDeliveryTrace(kind string, from, intendedRecipient, toName proto.DisplayName, to proto.PeerID) {
	intended := intendedRecipient
	if intended == "" {
		intended = toName
	}
	pane, _ := m.transport.ConnectionPaneID(to)
	log.Printf(
		"%s delivery trace: sender_identity=%s intended_recipient_name=%s resolved_peer_id=%s frame.to_peer=%s actual_delivered_pane_id=%s",
		kind, from, intended, to, toName, pane,
	)
}
