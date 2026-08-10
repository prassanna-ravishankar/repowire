package hub

// Session-wiring HTTP route group: the three "polling peer / stop hook" endpoints
// that close the messaging-wiring gaps left after the ask/notify ports.
//
//	POST /session/update      report status / turn_state / model / metadata
//	GET  /deliveries/pending  drain the durable queued-delivery queue
//
// Port of repowire/daemon/routes/messages.py (update_session, deliver_response)
// and asks.py (pending_deliveries). Wire shapes match the Python daemon
// byte-for-byte — CLI/bot/relay/hook clients depend on them.
//
// Identity discipline: routing-sensitive state resolves to proto.PeerID via the
// registry; status/turn_state are applied by peer_id (resolve first), model and
// metadata via the by-name updaters. Fail loud over silent degrade — a missing
// required field is a 400, an unknown peer a 404, a bad status a 400.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// sessionRegistry is the narrow registry seam the session routes need. It mirrors
// the PeerRegistry methods the Python update_session / deliver_response /
// pending_deliveries handlers call.
//
// The concrete registry satisfies this seam. Keeping it narrow makes the
// handlers independently testable without SQLite or a live transport.
type sessionRegistry interface {
	// GetPeerByPane resolves a tmux-pane-keyed transport to its peer.
	GetPeerByPane(pane string) (*proto.Peer, bool)
	// GetPeerByName resolves a display_name within an optional circle scope; err
	// is the ambiguous-name (ValueError) rejection.
	GetPeerByName(name string, circle *string) (*proto.Peer, error)
	// UpdateStatus / UpdateTurnState mutate by canonical peer_id (the wire status
	// frame path through the FSM). Resolve the identifier first.
	UpdateStatus(ctx context.Context, id proto.PeerID, status proto.PeerStatus) error
	UpdateTurnState(ctx context.Context, id proto.PeerID, ts proto.TurnState)
	// UpdateModelByName / UpdateMetadataByName resolve an addressing string and
	// patch the field, returning (found, err). err is the ambiguous-name reject.
	UpdateModelByName(ctx context.Context, identifier, model string) (found bool, err error)
	UpdateMetadataByName(ctx context.Context, identifier string, metadata map[string]any) (found bool, err error)
}

// sessionDeps bundles the deps the session routes compose. Wired onto the Hub via
// WithSessionRoutes; the store is the durable queued-delivery seam (nil → the
// /deliveries/pending endpoint returns an empty list, matching the Python
// getattr(state, "queued_delivery_store", None) is None early-return).
type sessionDeps struct {
	reg   sessionRegistry
	store queuedDrainStore
}

// queuedDrainStore is the drain seam for /deliveries/pending and the flush-on-
// connect path. *state.Store satisfies it via DrainDeliveries. nil → empty list.
type queuedDrainStore interface {
	DrainDeliveries(ctx context.Context, peerID string, maxResults int, now time.Time) ([]state.QueuedDelivery, error)
}

// WithSessionRoutes wires the session route group onto the hub. The concrete
// *peer.Registry satisfies sessionRegistry once the by-name updaters land; until
// then a test/fake registry is passed. store may be nil. Returns the receiver.
func (h *Hub) WithSessionRoutes(reg sessionRegistry, store queuedDrainStore) *Hub {
	h.session = &sessionDeps{reg: reg, store: store}
	return h
}

// registerSessionRoutes attaches the session endpoints, each behind requireAuth.
func (h *Hub) registerSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /session/update", h.requireAuth(h.handleSessionUpdate))
	mux.HandleFunc("GET /deliveries/pending", h.requireAuth(h.handleDeliveriesPending))
}

// sessionReady reports whether the session deps are wired; 503 otherwise.
func (h *Hub) sessionReady(w http.ResponseWriter) bool {
	if h.session == nil || h.session.reg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session routes not configured")
		return false
	}
	return true
}

// ----------------------------------------------------------------------------
// POST /session/update
// ----------------------------------------------------------------------------

// SessionUpdateRequest mirrors messages.py SessionUpdateRequest.
type SessionUpdateRequest struct {
	PeerName  *string        `json:"peer_name,omitempty"`
	PaneID    *string        `json:"pane_id,omitempty"`
	Status    *string        `json:"status,omitempty"`
	TurnState *string        `json:"turn_state,omitempty"`
	Model     *string        `json:"model,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// handleSessionUpdate updates a peer's status / turn_state / model / metadata.
// At least one of status/turn_state/model is required (400 otherwise). The peer
// is resolved by peer_name (wins) else pane_id (404 if no peer owns the pane).
// status is validated against online|busy|offline (400). status/turn_state apply
// by resolved peer_id; model/metadata via the by-name updaters. Mirrors
// messages.py update_session.
func (h *Hub) handleSessionUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.sessionReady(w) {
		return
	}
	var req SessionUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	if req.Status == nil && req.TurnState == nil && req.Model == nil {
		writeJSONError(w, http.StatusBadRequest,
			"At least one of status, turn_state, or model is required")
		return
	}

	// Validate status BEFORE resolving (a bad value is a request error regardless
	// of whether the peer exists), matching the Python ordering.
	var peerStatus proto.PeerStatus
	if req.Status != nil {
		switch proto.PeerStatus(*req.Status) {
		case proto.StatusOnline, proto.StatusBusy, proto.StatusOffline:
			peerStatus = proto.PeerStatus(*req.Status)
		default:
			writeJSONError(w, http.StatusBadRequest,
				"Invalid status: "+*req.Status+". Must be one of: online, busy, offline")
			return
		}
	}

	// Resolve the addressing identifier: peer_name wins, else pane_id.
	var identifier string
	var resolved proto.PeerID
	switch {
	case req.PeerName != nil && *req.PeerName != "":
		identifier = *req.PeerName
		// Resolve to peer_id for the status/turn_state by-id mutators. An
		// ambiguous name is a fail-loud 409; an unknown one a 404.
		p, err := h.session.reg.GetPeerByName(identifier, nil)
		if err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if p == nil {
			writeJSONError(w, http.StatusNotFound, "Unknown peer: "+identifier)
			return
		}
		resolved = p.PeerID
	case req.PaneID != nil && *req.PaneID != "":
		p, ok := h.session.reg.GetPeerByPane(*req.PaneID)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "No peer for pane: "+*req.PaneID)
			return
		}
		identifier = string(p.PeerID)
		resolved = p.PeerID
	default:
		writeJSONError(w, http.StatusBadRequest, "Either peer_name or pane_id required")
		return
	}

	if req.Status != nil {
		_ = h.session.reg.UpdateStatus(ctx, resolved, peerStatus)
	}
	if req.TurnState != nil {
		h.session.reg.UpdateTurnState(ctx, resolved, proto.TurnState(*req.TurnState))
	}
	if req.Model != nil {
		if _, err := h.session.reg.UpdateModelByName(ctx, identifier, *req.Model); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if len(req.Metadata) > 0 {
		if _, err := h.session.reg.UpdateMetadataByName(ctx, identifier, req.Metadata); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----------------------------------------------------------------------------
// GET /deliveries/pending
// ----------------------------------------------------------------------------

// PendingDelivery mirrors asks.py PendingDelivery.
type PendingDelivery struct {
	DeliveryID    string           `json:"delivery_id"`
	Kind          string           `json:"kind"`
	FromPeer      string           `json:"from_peer"`
	ToPeer        string           `json:"to_peer"`
	CorrelationID *string          `json:"correlation_id"`
	Text          string           `json:"text"`
	Attachments   []map[string]any `json:"attachments"`
	CreatedAt     string           `json:"created_at"`
	ExpiresAt     string           `json:"expires_at"`
}

// PendingDeliveriesResponse mirrors asks.py PendingDeliveriesResponse.
type PendingDeliveriesResponse struct {
	Deliveries []PendingDelivery `json:"deliveries"`
}

// handleDeliveriesPending drains the durable queued-delivery queue for one peer
// (delete-on-drain so a notify/ask paste is not replayed indefinitely). Lookup is
// by exactly one of pane_id or peer_id (400 otherwise); 404 if the peer is
// unknown. A nil store returns an empty list. Mirrors asks.py pending_deliveries.
func (h *Hub) handleDeliveriesPending(w http.ResponseWriter, r *http.Request) {
	if !h.sessionReady(w) {
		return
	}
	q := r.URL.Query()
	paneID := q.Get("pane_id")
	peerID := q.Get("peer_id")

	if paneID == "" && peerID == "" {
		writeJSONError(w, http.StatusBadRequest, "Must provide pane_id or peer_id")
		return
	}
	if paneID != "" && peerID != "" {
		writeJSONError(w, http.StatusBadRequest, "Provide only one of pane_id or peer_id")
		return
	}

	maxResults := 50
	if v := q.Get("max_results"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxResults = n
		}
	}

	// nil store → empty list (queue disabled), matching Python's None early-return.
	if h.session.store == nil {
		writeJSON(w, http.StatusOK, PendingDeliveriesResponse{Deliveries: []PendingDelivery{}})
		return
	}

	var resolved proto.PeerID
	if paneID != "" {
		p, ok := h.session.reg.GetPeerByPane(paneID)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "No peer for pane: "+paneID)
			return
		}
		resolved = p.PeerID
	} else {
		p, err := h.session.reg.GetPeerByName(peerID, nil)
		if err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if p == nil {
			writeJSONError(w, http.StatusNotFound, "No peer with id: "+peerID)
			return
		}
		resolved = p.PeerID
	}

	drained, err := h.session.store.DrainDeliveries(r.Context(), string(resolved), maxResults, time.Time{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := PendingDeliveriesResponse{Deliveries: make([]PendingDelivery, 0, len(drained))}
	for _, d := range drained {
		attachments := d.Attachments
		if attachments == nil {
			attachments = []map[string]any{}
		}
		out.Deliveries = append(out.Deliveries, PendingDelivery{
			DeliveryID:    d.DeliveryID,
			Kind:          string(d.Kind),
			FromPeer:      d.FromPeerName,
			ToPeer:        d.ToPeerName,
			CorrelationID: d.CorrelationID,
			Text:          d.Text,
			Attachments:   attachments,
			CreatedAt:     d.CreatedAt,
			ExpiresAt:     d.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
