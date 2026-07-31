package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// ============================================================================
// Messaging HTTP route group — POST /notify, POST /broadcast.
//
// Port of repowire/daemon/routes/messages.py (the notify/broadcast slice). The
// wire shapes (request + response JSON) match the Python daemon byte-for-byte
// because CLI/bot/relay clients depend on them. The handlers compose:
//
//   * LazyRepair (maintenance piggy-backs on a real request, never a timer)
//   * a created/resolved/terminal delivery-trace breadcrumb (notify only)
//   * service.PeerDelivery (registry access-check + transport choice + queue fallback)
//
// Error mapping mirrors the Python route, as JSON bodies (never a bare 500):
//   ambiguous peer       -> 409   unknown peer            -> 404
//   forbidden (circle)   -> 403   no-live-transport (503) -> records no_connection
//
// Transport CHOICE (ACP-before-WS) and the seed gate live in service.PeerDelivery; this
// layer is HTTP plumbing + truthful tracing.
// ============================================================================

// lazyRepairer is the narrow seam the messaging routes need from the registry:
// demand-driven maintenance kicked off in a TRACKED goroutine on every request
// (LazyRepairAsync, not a bare `go LazyRepair(...)`, so Registry.Close can join
// it before shutdown closes the Store). The concrete *peer.Registry satisfies
// it.
type lazyRepairer interface {
	LazyRepairAsync(ctx context.Context)
}

// deliveryTracer records delivery-trace stages. *state.Store satisfies it via
// RecordTrace. nil disables tracing (the route still works — tracing is a
// breadcrumb, not a delivery dependency).
//
// ponytail: the Go state.Store has RecordTrace but no record_outcome analogue
// yet, so the truthful-terminal-stage truth table (Python
// DeliveryTraceStore.record_outcome) is reproduced inline in recordOutcome
// below. Promote it onto *state.Store when another caller needs it.
type deliveryTracer interface {
	RecordTrace(ctx context.Context, traceID, kind, stage, status, deliveryID, peerID, fromPeerID string, detail map[string]any) error
}

// MessagingRoutes owns POST /notify and POST /broadcast. It depends on the
// delivery service (not the concrete registry) so the transport-choice and
// access-check policy stays in one place; reg is the LazyRepair seam and traces
// is the optional breadcrumb sink. Auth is supplied by the hub's requireAuth
// wrapper at registration so there is one bearer-token implementation.
type MessagingRoutes struct {
	delivery *service.PeerDelivery
	reg      lazyRepairer
	traces   deliveryTracer
}

// NewMessagingRoutes wires the messaging route group. traces may be nil (tracing
// disabled).
func NewMessagingRoutes(delivery *service.PeerDelivery, reg lazyRepairer, traces deliveryTracer) *MessagingRoutes {
	return &MessagingRoutes{delivery: delivery, reg: reg, traces: traces}
}

// Register attaches the messaging endpoints to the mux, each wrapped by the
// hub's auth middleware (the shared require_auth analogue).
func (mr *MessagingRoutes) Register(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /notify", auth(mr.handleNotify))
	mux.HandleFunc("POST /broadcast", auth(mr.handleBroadcast))
}

// ----------------------------------------------------------------------------
// Wire types — match daemon/routes/messages.py field-for-field.
// ----------------------------------------------------------------------------

type notifyRequest struct {
	FromPeer     string           `json:"from_peer"`
	ToPeer       string           `json:"to_peer"`
	Text         string           `json:"text"`
	Attachments  []map[string]any `json:"attachments"`
	BypassCircle bool             `json:"bypass_circle"`
	Circle       *string          `json:"circle"`
}

type notifyResponse struct {
	OK                    bool           `json:"ok"`
	Status                string         `json:"status"`
	DeliveryState         string         `json:"delivery_state"`
	Delivered             bool           `json:"delivered"`
	Queued                bool           `json:"queued"`
	Reason                string         `json:"reason"`
	FromPeerID            *string        `json:"from_peer_id"`
	FromPeerName          *string        `json:"from_peer_name"`
	ToPeerID              *string        `json:"to_peer_id"`
	ToPeerName            *string        `json:"to_peer_name"`
	RepowireSessionID     *string        `json:"repowire_session_id"`
	FromRepowireSessionID *string        `json:"from_repowire_session_id"`
	ToRepowireSessionID   *string        `json:"to_repowire_session_id"`
	HookDelivery          map[string]any `json:"hook_delivery"`
	DeliveryID            *string        `json:"delivery_id"`
}

type broadcastRequest struct {
	FromPeer     string   `json:"from_peer"`
	Text         string   `json:"text"`
	Exclude      []string `json:"exclude"`
	BypassCircle bool     `json:"bypass_circle"`
}

type broadcastResponse struct {
	OK     bool                `json:"ok"`
	SentTo []string            `json:"sent_to"`
	Failed []map[string]string `json:"failed"`
}

// ----------------------------------------------------------------------------
// POST /notify
// ----------------------------------------------------------------------------

func (mr *MessagingRoutes) handleNotify(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid JSON body: " + err.Error()})
		return
	}

	ctx := r.Context()
	// Maintenance piggy-backs on the request; never a timer.
	mr.reg.LazyRepairAsync(context.Background())

	// Mint the delivery id up front — it is also the trace id, recorded on the
	// "created" stage before the send so a trace exists even if delivery throws.
	deliveryID := "notif-delivery-" + uuid.NewString()[:8]
	mr.trace(ctx, deliveryID, "notify", "created", "", deliveryID, "", "", map[string]any{"to_peer": req.ToPeer})

	result, err := mr.delivery.Notify(ctx, service.NotifyParams{
		FromPeer:     req.FromPeer,
		ToPeer:       req.ToPeer,
		Text:         req.Text,
		BypassCircle: req.BypassCircle,
		Circle:       req.Circle,
		Attachments:  req.Attachments,
		DeliveryID:   deliveryID,
	})
	if err != nil {
		mr.writeNotifyError(w, ctx, req, deliveryID, err)
		return
	}

	// Truthful tracing: resolved_peer, an optional pending breadcrumb when the
	// no-live-transport fallback queued the message, then the terminal outcome
	// derived from the hook receipt (never assume injection).
	toPeerID := string(result.ToPeerID)
	fromPeerID := ""
	if result.FromPeerID != nil {
		fromPeerID = string(*result.FromPeerID)
	}
	mr.trace(ctx, deliveryID, "notify", "resolved_peer", "", deliveryID, toPeerID, fromPeerID, nil)
	if result.Queued() {
		mr.trace(ctx, deliveryID, "notify", "pending", "", deliveryID, toPeerID, fromPeerID, map[string]any{"reason": result.Reason})
	}
	if result.HookDelivery != nil || !result.Queued() {
		mr.recordOutcome(ctx, deliveryID, "notify", toPeerID, fromPeerID, result.Transport, result.HookDelivery)
	}

	writeJSON(w, http.StatusOK, notifyResponse{
		OK:                    true,
		Status:                result.Status,
		DeliveryState:         result.DeliveryState,
		Delivered:             result.Delivered(),
		Queued:                result.Queued(),
		Reason:                result.Reason,
		FromPeerID:            peerIDPtr(result.FromPeerID),
		FromPeerName:          nameStrPtr(result.FromPeerName),
		ToPeerID:              strPtr(string(result.ToPeerID)),
		ToPeerName:            nameStrPtr(result.ToPeerName),
		RepowireSessionID:     result.RepowireSessionID,
		FromRepowireSessionID: result.FromRepowireSessionID,
		ToRepowireSessionID:   result.ToRepowireSessionID,
		HookDelivery:          result.HookDelivery,
		DeliveryID:            strPtr(deliveryIfEmpty(result.DeliveryID, deliveryID)),
	})
}

// writeNotifyError maps a delivery error to the JSON body + status code the
// Python route returns. CheckAccess rejections are distinguished by message
// prefix ("Unknown peer" / "Ambiguous peer name"), matching the Python
// ValueError-string discrimination; a no-live-transport TransportError
// (service.ErrNotConnected) becomes a 503 with a no_connection trace breadcrumb.
func (mr *MessagingRoutes) writeNotifyError(w http.ResponseWriter, ctx context.Context, req notifyRequest, deliveryID string, err error) {
	msg := err.Error()
	switch {
	case isTransportUnavailable(err):
		mr.trace(ctx, deliveryID, "notify", "no_connection", "fail", deliveryID, "", "", map[string]any{"error": msg})
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":             false,
			"status":         "unavailable",
			"delivery_state": "no_live_transport",
			"delivered":      false,
			"queued":         false,
			"reason":         "no_live_transport",
			"detail":         "Peer " + req.ToPeer + " has no live connection: " + msg,
			"from_peer_name": req.FromPeer,
			"to_peer_name":   req.ToPeer,
		})
	case strings.HasPrefix(msg, "Ambiguous peer name"):
		writeJSON(w, http.StatusConflict, accessErrorBody("ambiguous_peer", "failed", "ambiguous_peer", msg, req))
	case strings.HasPrefix(msg, "Unknown peer"):
		writeJSON(w, http.StatusNotFound, accessErrorBody("not_found", "unknown_peer", "unknown_peer", msg, req))
	default:
		// Any other CheckAccess error is a circle/authorization rejection.
		writeJSON(w, http.StatusForbidden, accessErrorBody("forbidden", "failed", "forbidden", msg, req))
	}
}

func accessErrorBody(errStatus, deliveryState, reason, detail string, req notifyRequest) map[string]any {
	return map[string]any{
		"ok":             false,
		"status":         errStatus,
		"delivery_state": deliveryState,
		"delivered":      false,
		"queued":         false,
		"reason":         reason,
		"detail":         detail,
		"from_peer_name": req.FromPeer,
		"to_peer_name":   req.ToPeer,
	}
}

// ----------------------------------------------------------------------------
// POST /broadcast
// ----------------------------------------------------------------------------

func (mr *MessagingRoutes) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	var req broadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid JSON body: " + err.Error()})
		return
	}

	ctx := r.Context()
	mr.reg.LazyRepairAsync(context.Background())

	// Best-effort per-recipient: service.PeerDelivery.Broadcast never aborts the fanout
	// on a single failure, splits ACP recipients off the WS fanout (when the ACP
	// route is live), and defers pending_first_turn WS peers behind the seed gate.
	sent, failed := mr.delivery.Broadcast(ctx, req.FromPeer, req.Text, req.Exclude, req.BypassCircle)

	sentTo := make([]string, 0, len(sent))
	for _, n := range sent {
		sentTo = append(sentTo, string(n))
	}
	failedOut := make([]map[string]string, 0, len(failed))
	for _, f := range failed {
		failedOut = append(failedOut, map[string]string{"peer": string(f.PeerID), "error": f.Error})
	}

	writeJSON(w, http.StatusOK, broadcastResponse{OK: true, SentTo: sentTo, Failed: failedOut})
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

// trace records one delivery-trace stage best-effort (nil tracer = no-op). A
// trace write failure is logged-and-swallowed: tracing is a breadcrumb, not a
// delivery dependency.
func (mr *MessagingRoutes) trace(ctx context.Context, traceID, kind, stage, status, deliveryID, peerID, fromPeerID string, detail map[string]any) {
	if mr.traces == nil {
		return
	}
	_ = mr.traces.RecordTrace(ctx, traceID, kind, stage, status, deliveryID, peerID, fromPeerID, detail)
}

// recordOutcome reproduces DeliveryTraceStore.record_outcome's truth table: a
// hook receipt is the ONLY proof of pane injection — callers never hand-assert
// it. ws + status==injected -> hook_received + pane_injected; ws + failed/
// rejected -> hook_received + injection_failed; no receipt (ACP / legacy hook
// that never acks) -> websocket_sent (handoff only, verified=false).
func (mr *MessagingRoutes) recordOutcome(ctx context.Context, traceID, kind, peerID, fromPeerID, transport string, hookDelivery map[string]any) {
	if mr.traces == nil {
		return
	}
	rec := func(stage, status string, detail map[string]any) {
		_ = mr.traces.RecordTrace(ctx, traceID, kind, stage, status, traceID, peerID, fromPeerID, detail)
	}
	var hookStatus string
	if hookDelivery != nil {
		if s, ok := hookDelivery["status"].(string); ok {
			hookStatus = s
		}
	}
	switch hookStatus {
	case "injected":
		rec("hook_received", "", nil)
		rec("pane_injected", "", nil)
	case "failed", "rejected":
		rec("hook_received", "", nil)
		rec("injection_failed", "fail", map[string]any{"hook_status": hookStatus})
	default:
		rec("websocket_sent", "", map[string]any{"transport": transport, "verified": false})
	}
}

// isTransportUnavailable reports whether err is the no-live-transport rejection
// (service.ErrNotConnected from the WS transport, or a Notify that re-raised it because
// the queue was disabled). A *service.DeliveryInjectionError is NOT transport-
// unavailable — the socket is alive, the pane rejected the injection — but
// notify never raises that (only ask does), so it correctly falls through.
func isTransportUnavailable(err error) bool {
	if _, ok := service.AsDeliveryInjection(err); ok {
		return false
	}
	return errors.Is(err, service.ErrNotConnected) || errors.Is(err, service.ErrTransportUnavailable)
}

// strPtr is duplicated from service/session_control.go (which owns the
// canonical definition, shared with job_runner.go) because this route file is
// on the hub side of the hub/service split — not worth an exported seam for
// three lines.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nameStrPtr(n proto.DisplayName) *string {
	if n == "" {
		return nil
	}
	s := string(n)
	return &s
}

func peerIDPtr(id *proto.PeerID) *string {
	if id == nil {
		return nil
	}
	s := string(*id)
	return &s
}

func deliveryIfEmpty(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
