package hub

// Ask-lifecycle HTTP route group: the non-blocking ask/ack model plus the
// blocking /query compat shim. Port of repowire/daemon/routes/asks.py +
// messages.py (the /query handler) and ask_service.py (the ack/answer reply
// flow). Endpoints:
//
//	POST /ask                          register + deliver an ask
//	POST /ack                          close an ask (bare or with reply body)
//	POST /decline                      return an unhandled ask to its asker
//	POST /answer                       answer a structured-question ask
//	POST /query                        legacy blocking RPC (ask-based shim)
//	GET  /asks/pending                 the Stop-hook reminder source
//	POST /asks/{cid}/wait              bounded wait for resolution (wait_on_ack)
//
// Identity discipline: routing-sensitive lookups resolve to proto.PeerID via the
// registry / service.AskTracker; reply routing in /ack and /answer uses the STORED ask
// recipient (ask.ToPeerID), never the request's compat from_peer. Fail loud over
// silent-degrade: a service.DeliveryInjectionError is a 503 with the ask left for the
// route to close as send_failed (the peer is NOT marked unreachable — the socket
// is alive); an undeliverable ack reply is a 503 with the ask left OPEN for retry.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/service"
)

// askRoutesRegistry is the narrow registry seam the ask-lifecycle routes need.
// It mirrors the PeerRegistry methods the Python asks.py / messages.py handlers
// call (get_peer_by_pane, get_peer, get_peer-by-name, add_event).
//
// The concrete registry satisfies this seam. Keeping it narrow makes route
// tests hermetic (no SQLite and no live transport).
type askRoutesRegistry interface {
	// CheckAccess resolves the canonical sender/target pair and enforces circle
	// boundaries before an ask is registered.
	CheckAccess(ctx context.Context, fromPeer, toPeer string, bypassCircle bool, circle *string) (*proto.Peer, *proto.Peer, error)
	// GetPeerByPane resolves a tmux-pane-keyed transport (Claude Code / Codex
	// hooks) to its peer. (nil,false) when no peer owns the pane.
	GetPeerByPane(pane string) (*proto.Peer, bool)
	// GetPeer resolves by peer_id (the canonical key). (nil,false) when unknown.
	GetPeer(id proto.PeerID) (*proto.Peer, bool)
	// GetPeerByName resolves a display_name within an optional circle scope. Used
	// by /query's pre-check and /ask's sender/target resolution. err is the
	// ambiguous-name (ValueError) rejection.
	GetPeerByName(name string, circle *string) (*proto.Peer, error)
	// AddEvent records a journal event (query/response audit). Best-effort.
	AddEvent(ctx context.Context, typ string, payload map[string]any) (eventID string)
	// GetAllPeers snapshots every registered peer: the routing candidate set.
	GetAllPeers() []*proto.Peer
}

// descriptionClock is the optional registry capability that dates a peer's
// self-description (the concrete registry persists it).
type descriptionClock interface {
	DescriptionSetAt(id proto.PeerID) *time.Time
}

// askLifecycleDeps bundles the services the ask-lifecycle routes compose. Wired
// onto the Hub via WithAskLifecycle; nil-safe (handlers 503 when unwired).
type askLifecycleDeps struct {
	asks     *service.AskTracker
	askMany  *service.AskManyTracker
	delivery *service.PeerDelivery
	reg      askRoutesRegistry
}

// WithAskLifecycle wires the ask-lifecycle route dependencies onto the hub. The
// concrete *peer.Registry satisfies askRoutesRegistry once the registry port
// lands; until then a test/fake registry is passed. Returns the receiver.
func (h *Hub) WithAskLifecycle(asks *service.AskTracker, delivery *service.PeerDelivery, reg askRoutesRegistry) *Hub {
	h.ask = &askLifecycleDeps{asks: asks, askMany: service.NewAskManyTracker(asks), delivery: delivery, reg: reg}
	return h
}

func (h *Hub) traceAsk(ctx context.Context, cid, stage, status string, ask *service.Ask, detail map[string]any) {
	if h.deliveryTraces == nil {
		return
	}
	peerID, fromPeerID := "", ""
	if ask != nil {
		peerID = string(ask.ToPeerID)
		fromPeerID = string(ask.FromPeerID)
	}
	_ = h.deliveryTraces.RecordTrace(ctx, cid, "ask", stage, status, cid, peerID, fromPeerID, detail)
}

// traceAskClosed records the terminal stages. The closing reply text is kept
// on the closed stage (bounded) so `repowire why` can replay a thread from the
// ledger after the in-memory ask has been evicted.
func (h *Hub) traceAskClosed(ctx context.Context, ask *service.Ask, reason, reply string) {
	if ask == nil {
		return
	}
	h.traceAsk(ctx, ask.CorrelationID, "acked", "", ask, map[string]any{"close_reason": reason})
	detail := map[string]any{"close_reason": reason}
	if reply != "" {
		detail["reply"] = clipTraceText(reply)
	}
	h.closeOutcome(ctx, ask, detail)
	h.traceAsk(ctx, ask.CorrelationID, "closed", "", ask, detail)
}

// traceSendFailed records the terminal stage of an ask whose delivery failed,
// with the same outcome fields as any other close.
func (h *Hub) traceSendFailed(ctx context.Context, ask *service.Ask) {
	if ask == nil {
		return
	}
	detail := map[string]any{"close_reason": "send_failed"}
	h.closeOutcome(ctx, ask, detail)
	h.traceAsk(ctx, ask.CorrelationID, "closed", "fail", ask, detail)
}

// clipTraceText bounds free text stored in trace detail rows.
func clipTraceText(text string) string {
	const max = 500
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// askWaitMax mirrors ASK_WAIT_MAX_SECONDS: the hard cap on how long /asks/{cid}/wait
// holds a connection open. The client timeout must sit above this + margin.
const (
	askWaitMaxSeconds     = 50 * time.Second
	askWaitDefaultSeconds = 45 * time.Second
	defaultQueryTimeout   = 60 * time.Second
)

// registerAskLifecycleRoutes attaches the ask-lifecycle handlers to the mux. The
// Method-qualified patterns let ServeMux reject wrong methods before dispatch.
func (h *Hub) registerAskLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /ask", h.requireAuth(h.handleAsk))
	mux.HandleFunc("POST /ack", h.requireAuth(h.handleAck))
	mux.HandleFunc("POST /decline", h.requireAuth(h.handleDecline))
	mux.HandleFunc("POST /answer", h.requireAuth(h.handleAnswer))
	mux.HandleFunc("POST /questions/ask-blocking", h.requireAuth(h.handleAskBlockingQuestion))
	mux.HandleFunc("POST /query", h.requireAuth(h.handleQuery))
	mux.HandleFunc("POST /ask-many", h.requireAuth(h.handleAskMany))
	mux.HandleFunc("GET /ask-many/{parent_id}", h.requireAuth(h.handleAskManyResult))
	mux.HandleFunc("GET /asks/pending", h.requireAuth(h.handlePendingAsks))
	mux.HandleFunc("GET /asks/history", h.requireAuth(h.handleAskHistory))
	mux.HandleFunc("POST /asks/{correlation_id}/wait", h.requireAuth(h.handleAskWait))
}

type askBlockingOption struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type askBlockingRequest struct {
	Prompt         string              `json:"prompt"`
	Options        []askBlockingOption `json:"options"`
	Scope          string              `json:"scope"`
	TimeoutSeconds float64             `json:"timeout_seconds"`
	CorrelationID  string              `json:"correlation_id"`
	Origin         string              `json:"origin"`
	FromPeer       string              `json:"from_peer"`
	Metadata       map[string]any      `json:"metadata"`
}

func (h *Hub) handleAskBlockingQuestion(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req askBlockingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Prompt == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "prompt is required")
		return
	}
	if req.Scope == "tool_permission" && len(req.Options) == 0 {
		writeJSONError(w, http.StatusUnprocessableEntity, "tool_permission question requires at least one allow-capable option")
		return
	}
	wait := req.TimeoutSeconds
	if wait <= 0 {
		wait = 45
	}
	if wait > 55 {
		wait = 55
	}
	cid := req.CorrelationID
	if cid == "" {
		cid = "ask-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	}
	from := req.FromPeer
	if from == "" {
		from = "external"
	}
	options := make([]any, 0, len(req.Options))
	for _, option := range req.Options {
		options = append(options, map[string]any{"id": option.ID, "title": option.Title})
	}
	outcome := "timed_out"
	if req.Scope == "tool_permission" {
		outcome = "denied"
	}
	timeoutMessage := "blocking question timed out"
	question := map[string]any{"kind": "choice", "prompt": req.Prompt, "options": options, "blocking": true, "timeout_seconds": wait, "default_answer": map[string]any{"outcome": outcome, "message": timeoutMessage}, "scope": req.Scope, "metadata": req.Metadata}
	registered, err := h.ask.asks.Register(r.Context(), service.RegisterAskParams{FromPeerID: proto.PeerID(from), FromPeerName: proto.DisplayName(from), ToPeerID: proto.PeerID("__repowire_control__"), ToPeerName: proto.DisplayName("human"), Text: req.Prompt, CorrelationID: cid, Question: question, SkipHistory: true})
	if err != nil {
		if errors.Is(err, service.ErrQuiesced) {
			writeJSONError(w, http.StatusConflict, err.Error())
		} else {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	event := map[string]any{"from": from, "to": "human", "from_peer_id": from, "to_peer_id": "__repowire_control__", "text": req.Prompt, "correlation_id": registered, "question": question}
	if req.Origin != "" {
		event["origin"] = req.Origin
	}
	h.ask.reg.AddEvent(r.Context(), "ask", event)
	message := timeoutMessage
	answer, err := h.ask.asks.WaitForAnswer(r.Context(), registered, time.Duration(wait*float64(time.Second)), &service.Answer{Outcome: outcome, Message: &message})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"correlation_id": registered, "outcome": answer.Outcome, "option_id": answer.OptionID, "message": answer.Message})
}

// ----------------------------------------------------------------------------
// HTTP plumbing (package-shared). This route group is the canonical home of the
// hub's HTTP helpers — the requireAuth bearer-token wrapper, writeJSON,
// writeError, writeJSONError, and decodeJSON. Other route groups call
// h.requireAuth(handler) and reuse these writers without redeclaring them.
// ----------------------------------------------------------------------------

// requireAuth gates an HTTP handler behind the daemon bearer token. An empty
// configured token (h.authToken == "") disables auth (dev/local); same-origin
// requests from the daemon-served localhost dashboard are also allowed.
// Otherwise an "Authorization: Bearer <token>" header must match in constant
// time. Mirrors daemon/auth.py require_auth (401 on missing/invalid).
func (h *Hub) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authToken == "" || isLocalDashboardRequest(r) {
			next(w, r)
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) {
			writeError(w, http.StatusUnauthorized, "Missing authorization header")
			return
		}
		token := strings.TrimPrefix(got, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "Invalid auth token")
			return
		}
		next(w, r)
	}
}

func isLocalDashboardRequest(r *http.Request) bool {
	if !isLocalhost(r) {
		return false
	}
	host := r.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	referer, err := url.Parse(r.Referer())
	return err == nil && referer.Host != "" && strings.EqualFold(referer.Host, r.Host)
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a FastAPI-shaped error body ({"detail": <string>}) with the
// given status, matching the Python daemon's HTTPException wire shape.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"detail": detail})
}

// writeJSONError emits the {"detail": ...} envelope for a structured (map)
// detail. A plain string detail is routed through writeError.
func writeJSONError(w http.ResponseWriter, status int, detail any) {
	if s, ok := detail.(string); ok {
		writeError(w, status, s)
		return
	}
	writeJSON(w, status, map[string]any{"detail": detail})
}

// decodeJSON reads the request body into dst, 400 on malformed JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// askReady reports whether the ask-lifecycle deps are wired; 503 otherwise.
func (h *Hub) askReady(w http.ResponseWriter) bool {
	if h.ask == nil || h.ask.asks == nil || h.ask.askMany == nil || h.ask.delivery == nil || h.ask.reg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ask lifecycle not configured")
		return false
	}
	return true
}

// ----------------------------------------------------------------------------
// POST /ask
// ----------------------------------------------------------------------------

// AskRequest is the /ask body. Wire shape matches asks.py AskRequest.
type AskRequest struct {
	ReplyDelivery string `json:"reply_delivery,omitempty"` // pull callers retrieve replies over HTTP

	FromPeer string `json:"from_peer"`
	// ToPeer names the target. Leaving the key out (ToPeerOmitted) hands
	// the ask to the configured routing strategy; an empty or null to_peer
	// is a named ask to nobody and fails as an unknown peer.
	ToPeer        string           `json:"to_peer"`
	ToPeerOmitted bool             `json:"-"`
	Text          string           `json:"text"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
	ReplyTo       *string          `json:"reply_to,omitempty"`
	BypassCircle  bool             `json:"bypass_circle,omitempty"`
	Circle        *string          `json:"circle,omitempty"`
	Question      map[string]any   `json:"question,omitempty"`
	// FallbackTo is where a targetless ask goes when the strategy cannot
	// choose or its pick is refused.
	FallbackTo string `json:"fallback_to,omitempty"`
	// Origin names the surface (and its message id) an ask from the human
	// was typed on, so the reply can thread there.
	Origin *AskOrigin `json:"origin,omitempty"`
}

// AskOrigin is where the human typed an ask.
type AskOrigin struct {
	Surface string `json:"surface"`
	Ref     string `json:"ref,omitempty"`
}

// MarshalJSON leaves to_peer out of a targetless ask, so the request
// round-trips.
func (a AskRequest) MarshalJSON() ([]byte, error) {
	type plain AskRequest
	raw, err := json.Marshal(plain(a))
	if err != nil || !a.ToPeerOmitted {
		return raw, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, err
	}
	delete(keys, "to_peer")
	return json.Marshal(keys)
}

// UnmarshalJSON records whether to_peer was present at all: only an absent
// key makes an ask targetless. Presence follows the decoder's own key
// matching, so a request that decodes a target can never be targetless.
func (a *AskRequest) UnmarshalJSON(raw []byte) error {
	type plain AskRequest
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, (*plain)(a)); err != nil {
		return err
	}
	named, _, err := targetKey(keys, "to_peer")
	if err != nil {
		return err
	}
	a.ToPeerOmitted = !named && a.ToPeer == ""
	return nil
}

// targetKey reports whether a JSON object names its target field. present is
// true for any key encoding/json matches to field (it matches case-
// insensitively, so TO_PEER counts); null reports a JSON null there. A key
// that only looks like the field (toPeer, to-peer) is never decoded, so it
// is an error rather than a silent omission that would route the ask.
func targetKey(keys map[string]json.RawMessage, field string) (present, null bool, err error) {
	squash := strings.NewReplacer("_", "", "-", "")
	for key, value := range keys {
		switch {
		case strings.EqualFold(key, field):
			present = true
			null = null || string(bytes.TrimSpace(value)) == "null"
		case strings.EqualFold(squash.Replace(key), squash.Replace(field)):
			return false, false, fmt.Errorf("unrecognized key %q: the target field is %q", key, field)
		}
	}
	return present, null, nil
}

// AskResponse mirrors asks.py AskResponse. Routing is set only for targetless asks.
type AskResponse struct {
	CorrelationID string      `json:"correlation_id"`
	Error         *string     `json:"error,omitempty"`
	Routing       *AskRouting `json:"routing,omitempty"`
	// Inbox is set when the recipient is the human peer.
	Inbox *InboxReceipt `json:"inbox,omitempty"`
}

// InboxReceipt is the human inbox's synchronous receipt: the stored item and
// the surfaces a push was started on. Per-surface outcomes are trace stages.
type InboxReceipt struct {
	ItemID   string   `json:"item_id"`
	Surfaces []string `json:"surfaces"`
}

func inboxReceipt(r *service.InboxReceipt) *InboxReceipt {
	if r == nil {
		return nil
	}
	surfaces := r.Surfaces
	if surfaces == nil {
		surfaces = []string{}
	}
	return &InboxReceipt{ItemID: r.ItemID, Surfaces: surfaces}
}

func (h *Hub) askOperationReady() error {
	if h.ask == nil || h.ask.asks == nil || h.ask.delivery == nil || h.ask.reg == nil {
		return routeErr(http.StatusServiceUnavailable, "ask lifecycle not configured")
	}
	return nil
}

// handleAsk opens a non-blocking ask: resolve+authorize via CheckAccess, register
// in the service.AskTracker (minting ask-<hex8> or reusing the
// caller-supplied cid), then deliver. On service.ErrQuiesced → 409; on a
// service.DeliveryInjectionError → close send_failed + 503 {injection_failed}; on a
// genuine TransportError → close send_failed + 503 (the peer is marked offline
// inside DeliverAsk). reply_to closes the referenced prior ask on success.
func (h *Hub) handleAsk(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req AskRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.openAsk(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// openAsk is the typed ask operation shared by HTTP and MCP callers.
func (h *Hub) openAsk(ctx context.Context, req AskRequest) (AskResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return AskResponse{}, err
	}

	if req.ReplyDelivery != "" && req.ReplyDelivery != "push" && req.ReplyDelivery != "pull" {
		return AskResponse{}, routeErr(http.StatusUnprocessableEntity, "reply_delivery must be push or pull")
	}
	if !req.ToPeerOmitted {
		name, machine, err := h.remoteAddress(ctx, req.ToPeer)
		if err == nil {
			return h.openRemoteAsk(ctx, req, name, machine)
		}
		if !errors.Is(err, errNotRemote) {
			return AskResponse{}, err
		}
	}
	if req.ReplyTo != nil {
		if prior, ok := h.ask.asks.Get(*req.ReplyTo); ok && (proto.IsRemotePeerID(prior.FromPeerID) || proto.IsRemotePeerID(prior.ToPeerID)) {
			return AskResponse{}, routeErr(http.StatusUnprocessableEntity, "a cross-machine ask cannot be closed by reply_to; ack it instead")
		}
	}
	res, err := h.resolveAsk(ctx, req)
	if err != nil {
		return AskResponse{}, err
	}
	cid, target, fromID, fromName := res.cid, res.target, res.fromID, res.fromName
	// addressed is how hints name the recipient: as the sender named it, or
	// as the strategy chose it.
	addressed := req.ToPeer
	if res.selection != nil {
		addressed = string(target.DisplayName)
	}
	// Planned shadow arms run whatever happens to delivery, so a quiesced,
	// dead or rejecting recipient is still scored, not mistaken for a
	// deliberate non-sample.
	defer h.runShadow(cid, res.decision, res.slot, target.PeerID, fromID)

	registered, err := h.ask.asks.Register(ctx, service.RegisterAskParams{
		CorrelationID:         cid,
		FromPeerID:            fromID,
		FromPeerName:          fromName,
		ToPeerID:              target.PeerID,
		ToPeerName:            target.DisplayName,
		Text:                  req.Text,
		ReplyTo:               req.ReplyTo,
		FromRepowireSessionID: h.sessionIDForPeer(ctx, string(fromID)),
		ToRepowireSessionID:   h.sessionIDForPeer(ctx, string(target.PeerID)),
		Question:              req.Question,
		ReplyDelivery:         req.ReplyDelivery,
	})
	if err != nil {
		if errors.Is(err, service.ErrQuiesced) {
			h.recordQuiesced(ctx, res)
			return AskResponse{}, routeErr(http.StatusConflict, map[string]any{
				"error": "peer_switching",
				"hint":  fmt.Sprintf("Peer %s is mid-switch; retry shortly.", addressed),
			})
		}
		// Unreachable today (Register only refuses with ErrQuiesced); kept so a
		// future Register error fails loud instead of being swallowed.
		return AskResponse{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	cid = registered
	// The human's own ask is recorded before delivery, so an immediate answer
	// finds its thread anchor.
	if human, _ := h.inbox.Human(); human != "" && fromID == human {
		surface, ref := "", ""
		if req.Origin != nil {
			surface, ref = req.Origin.Surface, req.Origin.Ref
		}
		h.inbox.RecordOutbound(ctx, cid, target, req.Text, surface, ref)
	}
	tracked, _ := h.ask.asks.Get(cid)
	h.traceAsk(ctx, cid, "created", "", tracked, map[string]any{"to_peer": req.ToPeer, "from_peer": req.FromPeer, "text": clipTraceText(req.Text)})
	h.traceAsk(ctx, cid, "resolved_peer", "", tracked, nil)
	h.traceAsk(ctx, cid, "routed", "", tracked, nil)

	text := req.Text
	if res.selection != nil {
		text = routing.DeliveryNote(*res.selection, eligibleCount(res.decision)) + text
	}
	deliveryResult, err := h.ask.delivery.DeliverAsk(ctx, service.DeliverAskParams{
		FromPeer:      string(fromID),
		ToPeer:        string(target.PeerID),
		Text:          text,
		CorrelationID: cid,
		ReplyTo:       req.ReplyTo,
		BypassCircle:  true, // sender already authorized; deliver_ask bypasses re-gating (asks.py: bypass_circle=True)
		Circle:        req.Circle,
		Attachments:   req.Attachments,
		Question:      req.Question,
	})
	if err != nil {
		if di, ok := service.AsDeliveryInjection(err); ok {
			// Fail loud: hook reached, pane rejected. Record injection_failed and
			// 503; the socket is alive so the peer is NOT marked unreachable.
			h.ask.reg.AddEvent(ctx, "delivery_trace", map[string]any{
				"trace_id": cid, "kind": "ask", "stage": "injection_failed",
				"status": "fail", "detail": di.Detail, "hook_delivery": di.HookDelivery,
			})
			_, _ = h.ask.asks.Close(ctx, cid, "send_failed")
			recordDeliveryOutcome(ctx, h.deliveryTraces, cid, "ask", string(target.PeerID), string(fromID), "ws", di.HookDelivery)
			h.traceSendFailed(ctx, tracked)
			return AskResponse{}, routeErr(http.StatusServiceUnavailable, map[string]any{
				"error":          "injection_failed",
				"hint":           fmt.Sprintf("Ask injection failed for %s: %s", addressed, di.Error()),
				"correlation_id": cid,
			})
		}
		// Unknown target / circle violation surfaced by CheckAccess, or a genuine
		// no-connection TransportError. Either way the ask cannot stand: close it.
		_, _ = h.ask.asks.Close(ctx, cid, "send_failed")
		if errors.Is(err, service.ErrNotConnected) {
			h.traceAsk(ctx, cid, "no_connection", "fail", tracked, map[string]any{"error": err.Error()})
			h.traceSendFailed(ctx, tracked)
			return AskResponse{}, routeErr(http.StatusServiceUnavailable,
				fmt.Sprintf("Peer %s has no live connection: %s", addressed, err))
		}
		h.traceAsk(ctx, cid, "route_failed", "fail", tracked, map[string]any{"error": err.Error()})
		h.traceSendFailed(ctx, tracked)
		return AskResponse{}, routeErr(http.StatusNotFound, err.Error())
	}
	recordDeliveryOutcome(ctx, h.deliveryTraces, cid, "ask", string(target.PeerID), string(fromID), deliveryResult.Transport, deliveryResult.HookDelivery)

	// reply_to: close the referenced prior ask now that the new one landed.
	// TODO(trace-coverage): this close writes no closed trace row, so the
	// ledger cannot tell a reply_to close from an unanswered ask. Predates
	// the router; tracked in the routing schema notes.
	if req.ReplyTo != nil {
		_, _ = h.ask.asks.Close(ctx, *req.ReplyTo, "reply_to")
	}
	return AskResponse{CorrelationID: cid, Routing: routedSummary(res.selection, target), Inbox: inboxReceipt(deliveryResult.Inbox)}, nil
}

// resolveAskTarget resolves and authorizes an ask's target exactly as the
// explicit path always has, and records the declared target's resolution on
// decision. reason names the failure for the trace.
func (h *Hub) resolveAskTarget(ctx context.Context, req AskRequest, toPeer, declared string, decision *routing.Decision) (from, target *proto.Peer, reason string, err error) {
	// Resolve the target FIRST so the service.AskTracker entry is keyed on the canonical
	// peer_id (display names collide; PendingForPeer / reply routing are
	// peer_id-keyed). Mirrors AskService.open_ask resolving the peer before
	// register. An ambiguous name is a 409; an unknown one a 404.
	target, terr := h.ask.reg.GetPeerByName(toPeer, req.Circle)
	if terr != nil {
		declare(decision, declared, nil, routing.ResolutionAmbiguous)
		return nil, nil, routing.ResolutionAmbiguous, routeErr(http.StatusConflict, terr.Error())
	}
	if target == nil {
		declare(decision, declared, nil, routing.ResolutionUnknown)
		return nil, nil, routing.ResolutionUnknown, routeErr(http.StatusNotFound, "Unknown peer: "+toPeer)
	}
	if declared == toPeer {
		declare(decision, declared, target, routing.ResolutionResolved)
	} else if declared != "" {
		fallback, _ := h.ask.reg.GetPeerByName(declared, req.Circle)
		declare(decision, declared, fallback, routing.ResolutionResolved)
	}
	// Authorize the canonical target before registration. Delivery bypasses its
	// duplicate check below only because this shared seam has already enforced it.
	from, authorizedTarget, accessErr := h.ask.reg.CheckAccess(ctx, req.FromPeer, string(target.PeerID), req.BypassCircle, nil)
	if accessErr != nil {
		if err := h.notAddressableRouteErr(accessErr); err != nil {
			return from, nil, routing.AccessNotAddressable, err
		}
		return from, nil, "access_denied", routeErr(http.StatusForbidden, accessErr.Error())
	}
	target = authorizedTarget
	// Explicit gate before AskTracker.Register: a non-addressable target must
	// never leave a tracker entry or a queued delivery behind (409, not 403).
	if err := h.notAddressableRouteErr(proto.RequireAddressable(from, target)); err != nil {
		return from, nil, routing.AccessNotAddressable, err
	}
	// An unresolved sender is allowed for compatibility; preserve its supplied
	// identity in that case.
	fromID := proto.PeerID(req.FromPeer)
	if from != nil {
		fromID = from.PeerID
	}
	if fromID == target.PeerID {
		return from, nil, "self", routeErr(http.StatusUnprocessableEntity, "cannot ask the calling peer")
	}
	return from, target, "", nil
}

// ----------------------------------------------------------------------------
// POST /ack
// ----------------------------------------------------------------------------

// AckRequest mirrors asks.py AckRequest. from_peer is compat-only; reply routing
// uses the stored ask recipient.
type AckRequest struct {
	CorrelationID string           `json:"correlation_id"`
	Message       *string          `json:"message,omitempty"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
	FromPeer      *string          `json:"from_peer,omitempty"`
}

type AckResponse struct {
	OK bool `json:"ok"`
}

type DeclineRequest struct {
	CorrelationID string  `json:"correlation_id"`
	Reason        string  `json:"reason"`
	FromPeer      *string `json:"from_peer,omitempty"`
}

// handleAck closes an ask. Bare ack → Close(ack), idempotent re-ack of a closed
// ask → 200. A structured-question ask delegates to /answer. ack-with-message
// delivers the reply to the ORIGINAL asker first (framed "[ack #cid from
// @<recipient>] <msg>") and only closes ack_with_msg on success: 410 if the ask
// is already closed (reply undeliverable), 503 if the reply can't be delivered
// (ask stays open for retry). Mirrors AskService.ack.
func (h *Hub) handleAck(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req AckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.ackDirect(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ackDirect is the typed ack operation shared by HTTP and MCP callers.
func (h *Hub) ackDirect(ctx context.Context, req AckRequest) (AckResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return AckResponse{}, err
	}

	existing, ok := h.ask.asks.Get(req.CorrelationID)
	if !ok {
		return AckResponse{}, routeErr(http.StatusNotFound, "No open ask with correlation_id: "+req.CorrelationID)
	}
	if err := h.mayResolve(existing, req.FromPeer); err != nil {
		return AckResponse{}, err
	}

	hasBody := (req.Message != nil && *req.Message != "") || len(req.Attachments) > 0

	// Structured question, still open → /answer is the canonical verb.
	if existing.Question != nil && !existing.Closed {
		outcome := "acknowledged"
		if hasBody {
			outcome = "answered"
		}
		if _, err := h.answerDirect(ctx, AnswerRequest{
			CorrelationID: req.CorrelationID,
			Text:          req.Message,
			Outcome:       outcome,
			Attachments:   req.Attachments,
		}); err != nil {
			return AckResponse{}, err
		}
		return AckResponse{OK: true}, nil
	}

	// Already closed: a reply can no longer be delivered.
	if existing.Closed {
		if hasBody {
			return AckResponse{}, routeErr(http.StatusGone, fmt.Sprintf(
				"Ask %s is already closed; reply message was not delivered. "+
					"Send a new notify/ask instead.", req.CorrelationID))
		}
		// Idempotent bare re-ack.
		return AckResponse{OK: true}, nil
	}

	if hasBody {
		if err := h.deliverAskResolution(ctx, existing, "ack", "ack_with_msg", derefOr(req.Message, ""), req.Attachments); err != nil {
			return AckResponse{}, err
		}
		return AckResponse{OK: true}, nil
	}

	// Bare ack.
	if _, closed := h.ask.asks.Close(ctx, req.CorrelationID, "ack"); !closed {
		if err := h.heldOpen(req.CorrelationID); err != nil {
			return AckResponse{}, err
		}
	}
	h.emitAckEvent(ctx, existing, "ack", false, false, false)
	h.traceAskClosed(ctx, existing, "ack", "")
	return AckResponse{OK: true}, nil
}

func (h *Hub) handleDecline(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req DeclineRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.declineDirect(r.Context(), req); err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, AckResponse{OK: true})
}

func (h *Hub) declineDirect(ctx context.Context, req DeclineRequest) error {
	if err := h.askOperationReady(); err != nil {
		return err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return routeErr(http.StatusUnprocessableEntity, "reason is required")
	}
	existing, ok := h.ask.asks.Get(req.CorrelationID)
	if !ok {
		return routeErr(http.StatusNotFound, "No open ask with correlation_id: "+req.CorrelationID)
	}
	if err := h.mayResolve(existing, req.FromPeer); err != nil {
		return err
	}
	if existing.Closed {
		return routeErr(http.StatusGone, fmt.Sprintf("Ask %s is already closed", req.CorrelationID))
	}
	return h.deliverAskResolution(ctx, existing, "declined", "declined", req.Reason, nil)
}

// mayResolve lets only the ask's own recipient close a cross-machine ask:
// the source machine trusts that result as the target's. The local
// projection of an ask sent to another machine is settled only by that
// machine.
func (h *Hub) mayResolve(ask *service.Ask, fromPeer *string) error {
	if proto.IsRemotePeerID(ask.ToPeerID) {
		return routeErr(http.StatusForbidden, "this ask is answered on the other machine")
	}
	if !proto.IsRemotePeerID(ask.FromPeerID) {
		return nil
	}
	if fromPeer != nil {
		if p, _ := h.ask.reg.GetPeerByName(*fromPeer, nil); p != nil && p.PeerID == ask.ToPeerID {
			return nil
		}
	}
	return routeErr(http.StatusForbidden, "only the recipient may resolve a cross-machine ask")
}

// heldOpen explains a close the tracker refused: an ask still open was kept
// open by its durable result failing to commit.
func (h *Hub) heldOpen(cid string) error {
	if ask, ok := h.ask.asks.Get(cid); ok && !ask.Closed {
		return routeErr(http.StatusServiceUnavailable, "could not record the result; the ask stays open, retry")
	}
	return nil
}

func (h *Hub) deliverAskResolution(ctx context.Context, ask *service.Ask, frameKind, closeReason, message string, attachments []map[string]any) error {
	if ask.ReplyDelivery == "pull" {
		if _, closed := h.ask.asks.Resolve(ctx, ask.CorrelationID, closeReason, &message, attachments); !closed {
			if err := h.heldOpen(ask.CorrelationID); err != nil {
				return err
			}
		}
		h.emitAckEvent(ctx, ask, closeReason, true, true, len(attachments) > 0)
		h.traceAskClosed(ctx, ask, closeReason, message)
		return nil
	}

	framed := fmt.Sprintf("[%s #%s from @%s] %s", frameKind, ask.CorrelationID, ask.ToPeerName, message)
	threadText := message
	if frameKind == "declined" {
		threadText = "Declined: " + message
	}
	res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{
		FromPeer: string(ask.ToPeerID), ToPeer: string(ask.FromPeerID), Text: framed,
		BypassCircle: true, Attachments: attachments,
		CorrelationID: ask.CorrelationID, ThreadText: threadText,
	})
	if err != nil {
		if errors.Is(err, service.ErrNotConnected) {
			return routeErr(http.StatusServiceUnavailable, fmt.Sprintf(
				"Reply delivery failed for %s: %s. Ask remains open; retry when the asker reconnects.", ask.FromPeerName, err))
		}
		_, _ = h.ask.asks.Close(ctx, ask.CorrelationID, closeReason)
		h.emitAckEvent(ctx, ask, closeReason, false, true, len(attachments) > 0)
		h.traceAskClosed(ctx, ask, closeReason, message)
		return nil
	}
	if !res.Delivered() && !res.Queued() {
		return routeErr(http.StatusServiceUnavailable, fmt.Sprintf(
			"Reply delivery failed for %s: %s. Ask remains open; retry when the asker reconnects.", ask.FromPeerName, res.Reason))
	}
	h.ask.asks.CaptureReply(ctx, ask.CorrelationID, message, attachments)
	_, _ = h.ask.asks.Close(ctx, ask.CorrelationID, closeReason)
	h.emitAckEvent(ctx, ask, closeReason, res.Delivered(), true, len(attachments) > 0)
	h.traceAskClosed(ctx, ask, closeReason, message)
	return nil
}

// ----------------------------------------------------------------------------
// POST /answer
// ----------------------------------------------------------------------------

// AnswerRequest mirrors asks.py AnswerRequest.
type AnswerRequest struct {
	CorrelationID string           `json:"correlation_id"`
	OptionID      *string          `json:"option_id,omitempty"`
	Text          *string          `json:"text,omitempty"`
	Outcome       string           `json:"outcome,omitempty"`
	Message       *string          `json:"message,omitempty"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
}

type AnswerResponse struct {
	OK bool `json:"ok"`
}

// handleAnswer answers a structured-question ask: 404 unknown, 422 plain ask
// (use /ack) or invalid option, 410 already answered/closed. Records the typed
// answerDirect (resolving any blocking waiter), then best-effort notifies a
// human-readable form back to the asker.
func (h *Hub) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req AnswerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.answerDirect(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// answerDirect is the typed structured-answer operation shared by HTTP and MCP callers.
func (h *Hub) answerDirect(ctx context.Context, req AnswerRequest) (AnswerResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return AnswerResponse{}, err
	}
	existing, ok := h.ask.asks.Get(req.CorrelationID)
	if !ok {
		return AnswerResponse{}, routeErr(http.StatusNotFound, "No open ask with correlation_id: "+req.CorrelationID)
	}
	if existing.Question == nil {
		return AnswerResponse{}, routeErr(http.StatusUnprocessableEntity, fmt.Sprintf(
			"Ask %s is not a structured question; use /ack.", req.CorrelationID))
	}
	outcome := req.Outcome
	if outcome == "" {
		outcome = "answered"
	}
	ans := service.Answer{
		Outcome:  outcome,
		OptionID: req.OptionID,
		Text:     req.Text,
		Message:  req.Message,
	}
	recorded, err := h.ask.asks.Answer(ctx, req.CorrelationID, ans)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyAnswered) {
			return AnswerResponse{}, routeErr(http.StatusGone, fmt.Sprintf(
				"Ask %s is already answered/closed.", req.CorrelationID))
		}
		if errors.Is(err, service.ErrAskNotFound) {
			return AnswerResponse{}, routeErr(http.StatusNotFound,
				"No open ask with correlation_id: "+req.CorrelationID)
		}
		// Validation error (e.g. unknown option_id, choice w/o option).
		return AnswerResponse{}, routeErr(http.StatusUnprocessableEntity, err.Error())
	}

	// Best-effort deliver a human-readable form back to the asker. tool_permission
	// answers carry no body (the decision is consumed by the requesting transport,
	// not pasted to the asker). pull delivery is already satisfied by the waiter.
	body := answerReplyText(recorded, ans)
	isToolPermission := false
	if scope, _ := existing.Question["scope"].(string); scope == "tool_permission" {
		isToolPermission = true
	}
	delivered := false
	if isToolPermission || body == "" {
		delivered = true // nothing to push
	} else if existing.ReplyDelivery == "pull" {
		delivered = true // the resolved waiter returns the recorded answer
	} else {
		framed := fmt.Sprintf("[ack #%s from @%s] %s",
			req.CorrelationID, existing.ToPeerName, body)
		res, derr := h.ask.delivery.Notify(ctx, service.NotifyParams{
			FromPeer:      string(existing.ToPeerID),
			ToPeer:        string(existing.FromPeerID),
			Text:          framed,
			BypassCircle:  true,
			Attachments:   req.Attachments,
			CorrelationID: req.CorrelationID,
			ThreadText:    body,
		})
		// The answer is already recorded (first-answer-wins); a failed notify-back
		// is logged via the ack event, not surfaced as an error (the answer stands).
		delivered = derr == nil && res.Delivered()
	}
	h.emitAckEvent(ctx, existing, "answered", delivered, body != "", len(req.Attachments) > 0)
	h.traceAskClosed(ctx, existing, "answered", body)
	return AnswerResponse{OK: true}, nil
}

// answerReplyText resolves the human-readable reply body for an answer: explicit
// text wins, else the chosen option's title, else the message. Mirrors
// AskService._answer_reply_text.
func answerReplyText(ask *service.Ask, ans service.Answer) string {
	if ans.Text != nil && *ans.Text != "" {
		return *ans.Text
	}
	if ans.OptionID != nil && *ans.OptionID != "" && ask.Question != nil {
		if opts, ok := ask.Question["options"].([]any); ok {
			for _, o := range opts {
				if m, ok := o.(map[string]any); ok {
					if id, _ := m["id"].(string); id == *ans.OptionID {
						if title, _ := m["title"].(string); title != "" {
							return title
						}
						return *ans.OptionID
					}
				}
			}
		}
		return *ans.OptionID
	}
	if ans.Message != nil {
		return *ans.Message
	}
	return ""
}

// ----------------------------------------------------------------------------
// POST /query — legacy blocking RPC, ask-based shim (parity default).
// ----------------------------------------------------------------------------

// QueryRequest mirrors messages.py QueryRequest.
type QueryRequest struct {
	FromPeer     *string  `json:"from_peer,omitempty"`
	ToPeer       string   `json:"to_peer"`
	Text         string   `json:"text"`
	Timeout      *float64 `json:"timeout,omitempty"`
	BypassCircle bool     `json:"bypass_circle,omitempty"`
	Circle       *string  `json:"circle,omitempty"`
}

// QueryResponse mirrors messages.py QueryResponse.
type QueryResponse struct {
	Text   *string `json:"text,omitempty"`
	Error  *string `json:"error,omitempty"`
	Status *string `json:"status,omitempty"`
}

// handleQuery is the blocking RPC compat shim: pre-check the target's status
// (BUSY/OFFLINE/unknown short-circuit to {error,status}), then register a
// BLOCKING text-question ask (scope mesh_ask, default_answer outcome=timed_out)
// and WaitForAnswer up to the timeout. Maps outcome → {text|error}. Mirrors
// messages.py query_peer.
func (h *Hub) handleQuery(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req QueryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	fromPeer := "cli"
	if req.FromPeer != nil && *req.FromPeer != "" {
		fromPeer = *req.FromPeer
	}
	// CLI requests (no explicit from_peer) auto-bypass circles.
	bypass := req.BypassCircle || req.FromPeer == nil
	cid, decision := h.beginDecision(ctx, askAttempt{
		surface: routing.SurfaceQuery, fromPeer: fromPeer, circle: req.Circle, bypass: bypass, text: req.Text, question: true,
	}, false)

	target, err := h.ask.reg.GetPeerByName(req.ToPeer, req.Circle)
	if err != nil {
		declare(decision, req.ToPeer, nil, routing.ResolutionAmbiguous)
		h.recordRouteFailure(ctx, cid, decision, proto.PeerID(fromPeer), routing.ResolutionAmbiguous, err)
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(err.Error())})
		return
	}
	if target == nil {
		declare(decision, req.ToPeer, nil, routing.ResolutionUnknown)
		h.recordRouteFailure(ctx, cid, decision, proto.PeerID(fromPeer), routing.ResolutionUnknown, nil)
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr("Unknown peer: " + req.ToPeer)})
		return
	}
	declare(decision, req.ToPeer, target, routing.ResolutionResolved)
	switch target.Status {
	case proto.StatusBusy, proto.StatusOffline:
		h.recordRouteFailure(ctx, cid, decision, proto.PeerID(fromPeer), string(target.Status), nil)
	}
	switch target.Status {
	case proto.StatusBusy:
		writeJSON(w, http.StatusOK, QueryResponse{
			Error:  strPtr(fmt.Sprintf("Peer '%s' is busy", req.ToPeer)),
			Status: strPtr(string(proto.StatusBusy)),
		})
		return
	case proto.StatusOffline:
		writeJSON(w, http.StatusOK, QueryResponse{
			Error:  strPtr(fmt.Sprintf("Peer '%s' is offline", req.ToPeer)),
			Status: strPtr(string(proto.StatusOffline)),
		})
		return
	}

	timeout := defaultQueryTimeout
	if req.Timeout != nil && *req.Timeout > 0 {
		timeout = time.Duration(*req.Timeout * float64(time.Second))
	}

	var fromIDPtr *string
	if from, ferr := h.ask.reg.GetPeerByName(fromPeer, &target.Circle); ferr == nil && from != nil {
		s := string(from.PeerID)
		fromIDPtr = &s
		h.rebaseSender(decision, from, bypass)
	}
	h.ask.reg.AddEvent(ctx, "query", map[string]any{
		"from": fromPeer, "to": req.ToPeer, "text": req.Text,
		"from_peer_id": fromIDPtr, "to_peer_id": string(target.PeerID), "status": "pending",
	})

	timeoutMsg := "Timeout waiting for " + req.ToPeer
	question := map[string]any{
		"kind":            "text",
		"prompt":          req.Text,
		"blocking":        true,
		"timeout_seconds": timeout.Seconds(),
		"scope":           "mesh_ask",
		"metadata":        map[string]any{"compat": "query"},
		"default_answer":  map[string]any{"outcome": "timed_out", "message": timeoutMsg},
	}

	h.recordDecision(ctx, cid, decision, target, proto.PeerID(fromPeer))
	cid, err = h.ask.asks.Register(ctx, service.RegisterAskParams{
		CorrelationID: cid,
		FromPeerID:    proto.PeerID(fromPeer),
		FromPeerName:  proto.DisplayName(fromPeer),
		ToPeerID:      target.PeerID,
		ToPeerName:    target.DisplayName,
		Text:          req.Text,
		Question:      question,
		SkipHistory:   true, // legacy /query keys FromPeerID by display name and writes no "created" stage
	})
	if err != nil {
		if errors.Is(err, service.ErrQuiesced) {
			writeJSON(w, http.StatusOK, QueryResponse{
				Error: strPtr(fmt.Sprintf("Peer %s is mid-switch; retry shortly.", req.ToPeer)),
			})
			return
		}
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(err.Error())})
		return
	}

	if _, derr := h.ask.delivery.DeliverAsk(ctx, service.DeliverAskParams{
		FromPeer:      fromPeer,
		ToPeer:        string(target.PeerID),
		Text:          req.Text,
		CorrelationID: cid,
		BypassCircle:  bypass,
		Circle:        req.Circle,
		Question:      question,
	}); derr != nil {
		_, _ = h.ask.asks.Close(ctx, cid, "send_failed")
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(derr.Error())})
		return
	}

	defaultAns := service.Answer{Outcome: "timed_out", Message: &timeoutMsg}
	ans, werr := h.ask.asks.WaitForAnswer(ctx, cid, timeout, &defaultAns)
	if werr != nil {
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(werr.Error())})
		return
	}
	switch ans.Outcome {
	case "timed_out":
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(timeoutMsg)})
	case "cancelled":
		writeJSON(w, http.StatusOK, QueryResponse{Error: strPtr(derefOr(ans.Message, "Query cancelled"))})
	default:
		text := ""
		if ans.Text != nil {
			text = *ans.Text
		} else if ans.Message != nil {
			text = *ans.Message
		} else if ans.OptionID != nil {
			text = *ans.OptionID
		}
		writeJSON(w, http.StatusOK, QueryResponse{Text: strPtr(text)})
	}
}

// ----------------------------------------------------------------------------
// POST /ask-many + GET /ask-many/{parent_id}
// ----------------------------------------------------------------------------

type AskManyRequest struct {
	FromPeer       string   `json:"from_peer"`
	ToPeers        []string `json:"to_peers"`
	Text           string   `json:"text"`
	Circle         *string  `json:"circle,omitempty"`
	BypassCircle   bool     `json:"bypass_circle,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type AskManyChildResponse struct {
	Peer          string  `json:"peer"`
	CorrelationID *string `json:"correlation_id"`
	Delivered     bool    `json:"delivered"`
	Error         *string `json:"error"`
}

type AskManyResponse struct {
	ParentID string                 `json:"parent_id"`
	Children []AskManyChildResponse `json:"children"`
}

func (h *Hub) handleAskMany(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req AskManyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.openAskMany(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// openAskMany is the typed fan-out operation shared by HTTP and MCP callers.
// notAddressableRouteErr maps proto.NotAddressableError to the 409
// peer_not_addressable body, carrying the parent peer when it is registered so
// the caller can reroute by hand. nil for any other error.
func (h *Hub) notAddressableRouteErr(err error) error {
	var na *proto.NotAddressableError
	if !errors.As(err, &na) {
		return nil
	}
	body := map[string]any{
		"error": "peer_not_addressable", "peer_id": na.PeerID, "display_name": na.DisplayName,
		"reason": na.Reason, "parent_peer_id": nil,
		"hint": "This peer cannot receive direct input (for example a Codex multi-agent sub-agent thread). Send to its parent or another addressable peer; nothing was queued.",
	}
	if h.reg != nil && na.ParentRuntimeID != "" {
		if parent := h.reg.ParentPeer(&proto.Peer{PeerID: na.PeerID, Provenance: proto.Provenance{ParentRuntimeID: na.ParentRuntimeID}}); parent != nil {
			body["parent_peer_id"] = parent.PeerID
		}
	}
	return routeErr(http.StatusConflict, body)
}

func (h *Hub) openAskMany(ctx context.Context, req AskManyRequest) (AskManyResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return AskManyResponse{}, err
	}
	if len(req.ToPeers) == 0 {
		return AskManyResponse{}, routeErr(http.StatusUnprocessableEntity, "to_peers must not be empty")
	}
	if len(req.ToPeers) > service.MaxAskManyPeers {
		return AskManyResponse{}, routeErr(http.StatusUnprocessableEntity,
			fmt.Sprintf("to_peers exceeds the %d-peer limit", service.MaxAskManyPeers))
	}
	parent := h.ask.askMany.Create(req.FromPeer, req.Text, req.TimeoutSeconds)
	out := AskManyResponse{ParentID: parent.ParentID}
	seenNames := map[string]struct{}{}
	seenPeerIDs := map[proto.PeerID]struct{}{}

	for _, toPeer := range req.ToPeers {
		if _, ok := seenNames[toPeer]; ok {
			continue
		}
		seenNames[toPeer] = struct{}{}

		cid, decision := h.beginDecision(ctx, askAttempt{
			surface: routing.SurfaceAskMany, fromPeer: req.FromPeer, circle: req.Circle,
			bypass: req.BypassCircle, text: req.Text, parentID: &parent.ParentID,
		}, false)
		target, terr := h.ask.reg.GetPeerByName(toPeer, req.Circle)
		if terr != nil || target == nil {
			msg := "peer not found: " + toPeer
			resolution := routing.ResolutionUnknown
			if terr != nil {
				msg = terr.Error()
				resolution = routing.ResolutionAmbiguous
			}
			declare(decision, toPeer, nil, resolution)
			h.recordRouteFailure(ctx, cid, decision, proto.PeerID(req.FromPeer), resolution, errors.New(msg))
			h.ask.askMany.AddChild(parent.ParentID, service.AskManyChild{PeerName: toPeer, DeliveryError: &msg})
			out.Children = append(out.Children, AskManyChildResponse{Peer: toPeer, Error: &msg})
			continue
		}
		if _, ok := seenPeerIDs[target.PeerID]; ok {
			continue
		}
		seenPeerIDs[target.PeerID] = struct{}{}

		fromID := proto.PeerID(req.FromPeer)
		fromName := proto.DisplayName(req.FromPeer)
		if from, ferr := h.ask.reg.GetPeerByName(req.FromPeer, &target.Circle); ferr == nil && from != nil {
			fromID = from.PeerID
			fromName = from.DisplayName
			h.rebaseSender(decision, from, req.BypassCircle)
		}
		declare(decision, toPeer, target, routing.ResolutionResolved)
		if fromID == target.PeerID {
			msg := "cannot ask the calling peer"
			h.recordRouteFailure(ctx, cid, decision, fromID, "self", nil)
			h.ask.askMany.AddChild(parent.ParentID, service.AskManyChild{PeerName: string(target.DisplayName), PeerID: strPtr(string(target.PeerID)), DeliveryError: &msg})
			out.Children = append(out.Children, AskManyChildResponse{Peer: string(target.DisplayName), Error: &msg})
			continue
		}
		h.recordDecision(ctx, cid, decision, target, fromID)
		cid, err := h.ask.asks.Register(ctx, service.RegisterAskParams{
			CorrelationID: cid,
			FromPeerID:    fromID,
			FromPeerName:  fromName,
			ToPeerID:      target.PeerID,
			ToPeerName:    target.DisplayName,
			Text:          req.Text,
			ParentID:      &parent.ParentID,
			SkipHistory:   true, // children write no "created" stage
		})
		if err != nil {
			msg := err.Error()
			h.ask.askMany.AddChild(parent.ParentID, service.AskManyChild{
				PeerName: string(target.DisplayName), PeerID: strPtr(string(target.PeerID)), DeliveryError: &msg,
			})
			out.Children = append(out.Children, AskManyChildResponse{Peer: string(target.DisplayName), Error: &msg})
			continue
		}

		delivered := true
		var errText *string
		if _, err := h.ask.delivery.DeliverAsk(ctx, service.DeliverAskParams{
			FromPeer:      string(fromID),
			ToPeer:        string(target.PeerID),
			Text:          req.Text,
			CorrelationID: cid,
			BypassCircle:  req.BypassCircle,
			Circle:        req.Circle,
		}); err != nil {
			_, _ = h.ask.asks.Close(ctx, cid, "send_failed")
			msg := err.Error()
			errText = &msg
			delivered = false
		}
		h.ask.askMany.AddChild(parent.ParentID, service.AskManyChild{
			PeerName:      string(target.DisplayName),
			CorrelationID: &cid,
			PeerID:        strPtr(string(target.PeerID)),
			DeliveryError: errText,
		})
		out.Children = append(out.Children, AskManyChildResponse{
			Peer: string(target.DisplayName), CorrelationID: &cid, Delivered: delivered, Error: errText,
		})
	}
	return out, nil
}

func (h *Hub) handleAskManyResult(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	parentID := r.PathValue("parent_id")
	if parentID == "" || strings.Contains(parentID, "/") {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	out, err := h.askManyResult(parentID)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// askManyResult returns the typed fan-out status for HTTP and MCP callers.
func (h *Hub) askManyResult(parentID string) (map[string]any, error) {
	if err := h.askOperationReady(); err != nil {
		return nil, err
	}
	if parentID == "" || strings.Contains(parentID, "/") {
		return nil, routeErr(http.StatusNotFound, "not found")
	}
	out, ok := h.ask.askMany.Status(parentID, time.Time{})
	if !ok {
		return nil, routeErr(http.StatusNotFound, "ask-many "+parentID+" not found")
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// GET /asks/pending — the Stop-hook reminder source.
// ----------------------------------------------------------------------------

// PendingAsk mirrors asks.py PendingAsk.
type PendingAsk struct {
	CorrelationID string `json:"correlation_id"`
	FromPeer      string `json:"from_peer"`
	ToPeer        string `json:"to_peer"`
	Text          string `json:"text"`
	CreatedAt     string `json:"created_at"`
	Direction     string `json:"direction"`
}

// PendingAsksResponse mirrors asks.py PendingAsksResponse.
type PendingAsksResponse struct {
	Asks []PendingAsk `json:"asks"`
}

// handlePendingAsks returns open asks for a peer (newest first). Lookup is by
// exactly one of pane_id or peer_id (400 otherwise); 404 if the peer is unknown.
// direction ∈ {inbound(default),outbound,both}. Mirrors asks.py pending_asks.
func (h *Hub) handlePendingAsks(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	q := r.URL.Query()
	paneID := q.Get("pane_id")
	peerID := q.Get("peer_id")
	direction := q.Get("direction")
	if direction == "" {
		direction = "inbound"
	}

	if paneID == "" && peerID == "" {
		writeJSONError(w, http.StatusBadRequest, "Must provide pane_id or peer_id")
		return
	}
	if paneID != "" && peerID != "" {
		writeJSONError(w, http.StatusBadRequest, "Provide only one of pane_id or peer_id")
		return
	}
	switch direction {
	case "inbound", "outbound", "both":
	default:
		writeJSONError(w, http.StatusBadRequest, "direction must be one of: inbound, outbound, both")
		return
	}

	var resolved proto.PeerID
	if paneID != "" {
		p, ok := h.ask.reg.GetPeerByPane(paneID)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "No peer for pane: "+paneID)
			return
		}
		resolved = p.PeerID
	} else {
		p, ok := h.ask.reg.GetPeer(proto.PeerID(peerID))
		if !ok {
			// HTTP callers may open asks without registering a live peer.
			// They can retrieve their outbound asks using the same identity.
			if direction != "outbound" {
				writeJSONError(w, http.StatusNotFound, "No peer with id: "+peerID)
				return
			}
			resolved = proto.PeerID(peerID)
		} else {
			resolved = p.PeerID
		}
	}
	// The asker's own hooks are the request that repairs its cross-machine
	// asks, whichever direction this call lists.
	h.reconcileRemote(r.Context(), resolved)

	// maxResults<0 → uncapped, matching the Python default (no cap on the poll).
	pending, err := h.ask.asks.PendingForPeer(r.Context(), resolved, -1, direction)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := PendingAsksResponse{Asks: make([]PendingAsk, 0, len(pending))}
	for _, ask := range pending {
		dir := "outbound"
		if ask.ToPeerID == resolved {
			dir = "inbound"
		}
		out.Asks = append(out.Asks, PendingAsk{
			CorrelationID: ask.CorrelationID,
			FromPeer:      string(ask.FromPeerName),
			ToPeer:        string(ask.ToPeerName),
			Text:          ask.Text,
			CreatedAt:     ask.CreatedAt.Format(time.RFC3339Nano),
			Direction:     dir,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------------------------------------------------------------------
// /asks/{correlation_id}/wait
// ----------------------------------------------------------------------------

// AskWaitRequest mirrors asks.py AskWaitRequest.
type AskWaitRequest struct {
	PeerID         string   `json:"peer_id"`
	TimeoutSeconds *float64 `json:"timeout_seconds,omitempty"`
}

// AskWaitResponse mirrors asks.py AskWaitResponse.
type AskWaitResponse struct {
	CorrelationID string           `json:"correlation_id"`
	Status        string           `json:"status"` // "resolved" | "pending"
	Reply         *string          `json:"reply,omitempty"`
	Outcome       *string          `json:"outcome,omitempty"`
	OptionID      *string          `json:"option_id,omitempty"`
	Message       *string          `json:"message,omitempty"`
	CloseReason   *string          `json:"close_reason,omitempty"`
	Responder     *string          `json:"responder,omitempty"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
}

// handleAskWait blocks (bounded) until the ask resolves; pending on timeout. Only
// the original asker may wait (waiting flips the ask to pull delivery): 404 if
// the ask is unknown, 403 if peer_id is neither the asker's id nor name. Clamps
// the wait to askWaitMaxSeconds. Mirrors asks.py wait_on_ask.
func (h *Hub) handleAskWait(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req AskWaitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > askWaitMaxSeconds.Seconds() {
		clamped := askWaitMaxSeconds.Seconds()
		req.TimeoutSeconds = &clamped
	}
	result, err := h.waitOnAck(r.Context(), r.PathValue("correlation_id"), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// waitOnAck blocks for a tracked ask using the same bounded pull-delivery
// semantics as the HTTP endpoint.
func (h *Hub) waitOnAck(ctx context.Context, cid string, req AskWaitRequest) (AskWaitResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return AskWaitResponse{}, err
	}
	timeout := askWaitDefaultSeconds
	if req.TimeoutSeconds != nil {
		timeout = time.Duration(*req.TimeoutSeconds * float64(time.Second))
	}
	if timeout < 0 {
		timeout = 0
	}
	// A cross-machine ask is answered from its durable row, which outlives
	// the tracker projection.
	if h.remote != nil {
		if row, err := h.store.GetOutbound(ctx, cid); err == nil {
			return h.waitRemote(ctx, row, req.PeerID, timeout)
		}
	}
	existing, ok := h.ask.asks.Get(cid)
	if !ok {
		return AskWaitResponse{}, routeErr(http.StatusNotFound, "No ask with correlation_id: "+cid)
	}
	if req.PeerID != string(existing.FromPeerID) && req.PeerID != string(existing.FromPeerName) {
		return AskWaitResponse{}, routeErr(http.StatusForbidden, map[string]any{
			"error":          "not_the_asker",
			"correlation_id": cid,
			"asker":          string(existing.FromPeerName),
		})
	}

	ask, err := h.ask.asks.WaitForResolution(ctx, cid, timeout, true)
	if err != nil {
		if errors.Is(err, service.ErrAskNotFound) {
			return AskWaitResponse{}, routeErr(http.StatusNotFound, "No ask with correlation_id: "+cid)
		}
		return AskWaitResponse{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	if ask == nil {
		return AskWaitResponse{CorrelationID: cid, Status: "pending"}, nil
	}

	resp := AskWaitResponse{
		CorrelationID: cid,
		Status:        "resolved",
		CloseReason:   strPtr(ask.CloseReason),
		Responder:     strPtr(string(ask.ToPeerName)),
		Attachments:   ask.ReplyAttachments,
	}
	// reply: prefer the captured reply_text, else the answer text.
	if ask.ReplyText != nil {
		resp.Reply = ask.ReplyText
	} else if ask.Answer != nil {
		resp.Reply = ask.Answer.Text
	}
	if ask.Answer != nil {
		resp.Outcome = strPtr(ask.Answer.Outcome)
		resp.OptionID = ask.Answer.OptionID
		resp.Message = ask.Answer.Message
	}
	return resp, nil
}

// ----------------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------------

// emitAckEvent records the "ack" journal event with the truthful delivered flag,
// mirroring AskService._emit_ack_event. The from/to are swapped relative to the
// ask (the acker is the ask recipient replying to the original asker).
func (h *Hub) emitAckEvent(ctx context.Context, ask *service.Ask, reason string, delivered, hasMessage, hasAttachments bool) {
	h.ask.reg.AddEvent(ctx, "ack", map[string]any{
		"from":                     string(ask.ToPeerName),
		"to":                       string(ask.FromPeerName),
		"from_peer_id":             string(ask.ToPeerID),
		"to_peer_id":               string(ask.FromPeerID),
		"correlation_id":           ask.CorrelationID,
		"status":                   reason,
		"delivered":                delivered,
		"has_message":              hasMessage,
		"has_attachments":          hasAttachments,
		"repowire_session_id":      ask.FromRepowireSessionID,
		"from_repowire_session_id": ask.ToRepowireSessionID,
		"to_repowire_session_id":   ask.FromRepowireSessionID,
	})
}

func (h *Hub) sessionIDForPeer(ctx context.Context, peerID string) *string {
	if h.store == nil || peerID == "" {
		return nil
	}
	bindings, err := h.store.ListBindingsByPeer(ctx, peerID)
	if err != nil || len(bindings) == 0 {
		return nil
	}
	id := bindings[0].RepowireSessionID
	return &id
}

func derefOr(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

// ----------------------------------------------------------------------------
// GET /asks/history
// ----------------------------------------------------------------------------

// AskHistoryResponse lists the closed ask threads a peer took part in since a
// point in time, plus the peer's durable session id. Backs the git-trailer
// PreToolUse hook and `repowire why`.
type AskHistoryResponse struct {
	PeerID            string   `json:"peer_id"`
	RepowireSessionID *string  `json:"repowire_session_id"`
	Threads           []string `json:"threads"`
}

// handleAskHistory reads the durable trace ledger, not the in-memory tracker,
// so threads survive the AskTracker TTL. Identify the peer with exactly one of
// pane_id or peer_id; `since` is RFC-3339 and optional.
func (h *Hub) handleAskHistory(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	if h.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "trace ledger unavailable")
		return
	}
	q := r.URL.Query()
	paneID, peerID := q.Get("pane_id"), q.Get("peer_id")
	if (paneID == "") == (peerID == "") {
		writeJSONError(w, http.StatusBadRequest, "Provide exactly one of pane_id or peer_id")
		return
	}
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "since must be RFC-3339")
			return
		}
		since = parsed
	}
	var resolved proto.PeerID
	if paneID != "" {
		p, ok := h.ask.reg.GetPeerByPane(paneID)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "No peer for pane: "+paneID)
			return
		}
		resolved = p.PeerID
	} else {
		p, ok := h.ask.reg.GetPeer(proto.PeerID(peerID))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "No peer with id: "+peerID)
			return
		}
		resolved = p.PeerID
	}
	threads, err := h.store.ClosedAskThreadsSince(r.Context(), string(resolved), since)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if threads == nil {
		threads = []string{}
	}
	writeJSON(w, http.StatusOK, AskHistoryResponse{
		PeerID: string(resolved), RepowireSessionID: h.sessionIDForPeer(r.Context(), string(resolved)), Threads: threads,
	})
}
