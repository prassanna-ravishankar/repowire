package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// routes_events.go owns the "events (read + chat ingest)" HTTP route group:
//
//	GET  /events            — buffered dashboard event window (gap-recoverable)
//	POST /events/chat       — finalised chat turn from the Stop hook
//	POST /events/chat_delta — streaming partial chat block from the transcript tailer
//
// These mirror the Python daemon.routes.messages handlers (NOT app.py). The
// request/response JSON matches the Python wire shapes exactly — clients (the
// ws-hook poster, the per-pane transcript tailer, the dashboard) depend on it.
//
// The handlers resolve a peer for event scoping the same way Python does:
// explicit peer_id wins, else pane_id → GetPeerByPane (canonicalising the event's
// "peer" to the registered display_name), else a best-effort lookup by the
// supplied "peer" identifier. Resolution is best-effort: an unresolved peer is
// not an error — the event is still recorded so the dashboard never silently
// loses a turn. Auth uses the shared requireAuth gate; JSON I/O uses the shared
// writeJSON/writeJSONError helpers (routes_ask_lifecycle.go / routes_messaging.go).

// okResponse is the shared {"ok": true} body, mirroring the Python OkResponse.
// Defined here and reused by the lifecycle route group.
type okResponse struct {
	OK bool `json:"ok"`
}

// EventRoutes registers the events read + chat ingest endpoints on the mux, each
// behind the shared bearer-token gate. Kept separate from Hub.Routes so the route
// group is wired with one line and the base mux registration is untouched.
func (h *Hub) EventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /events", h.requireAuth(h.getEvents))
	mux.HandleFunc("POST /events/chat", h.requireAuth(h.ingestChatTurn))
	mux.HandleFunc("POST /events/chat_delta", h.requireAuth(h.ingestChatTurnDelta))
}

// getEvents handles GET /events?since=<event_id>.
//
// Without `since`: the full buffered window (last 500). With `since`: events
// after that id, or the full window if the id was evicted (gap-recovery
// fallback). Returns a JSON array of event maps.
func (h *Hub) getEvents(w http.ResponseWriter, r *http.Request) {
	h.reg.LazyRepairAsync(context.Background())

	since := r.URL.Query().Get("since")
	var events []map[string]any
	if since == "" {
		events = h.reg.GetEvents()
	} else {
		events = h.reg.EventsSince(since)
	}
	if events == nil {
		events = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, events)
}

// toolCallInfo mirrors the Python ToolCallInfo wire shape.
type toolCallInfo struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// chatTurnRequest mirrors the Python ChatTurnRequest. role ∈ {user, assistant}.
type chatTurnRequest struct {
	Peer      string         `json:"peer"`
	Role      string         `json:"role"`
	Text      string         `json:"text"`
	SessionID *string        `json:"session_id,omitempty"`
	ToolCalls []toolCallInfo `json:"tool_calls,omitempty"`
	TurnID    *string        `json:"turn_id,omitempty"`
	PeerID    *string        `json:"peer_id,omitempty"`
	PaneID    *string        `json:"pane_id,omitempty"`
}

// ingestChatTurn handles POST /events/chat: a finalised chat turn from the Stop
// hook for dashboard display.
//
// Peer resolution precedence (Python parity): explicit peer_id → pane_id (which
// canonicalises the event "peer" to the registered display_name) → the supplied
// "peer" identifier. An assistant turn_id is marked finalised in a bounded FIFO
// set so a late chat_delta for the same turn is dropped. The resolved event is
// recorded via reg.AddEvent("chat_turn", data).
func (h *Hub) ingestChatTurn(w http.ResponseWriter, r *http.Request) {
	var req chatTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, "malformed chat turn: "+err.Error())
		return
	}
	if req.Role != "user" && req.Role != "assistant" {
		writeJSONError(w, http.StatusUnprocessableEntity, "role must be one of: user, assistant")
		return
	}

	// data is the event payload; pane_id is resolution-only and excluded (Python
	// model_dump(exclude={"pane_id"})).
	data := map[string]any{
		"peer": req.Peer,
		"role": req.Role,
		"text": req.Text,
	}
	if req.SessionID != nil {
		data["session_id"] = *req.SessionID
	}
	if req.ToolCalls != nil {
		data["tool_calls"] = req.ToolCalls
	}
	if req.TurnID != nil {
		data["turn_id"] = *req.TurnID
	}
	if req.PeerID != nil {
		data["peer_id"] = *req.PeerID
	}

	var resolved *proto.Peer
	switch {
	case (req.PeerID == nil || *req.PeerID == "") && req.PaneID != nil && *req.PaneID != "":
		if p, ok := h.reg.GetPeerByPane(*req.PaneID); ok {
			resolved = p
			data["peer_id"] = string(p.PeerID)
			data["peer"] = string(p.DisplayName) // canonicalise to registered name
		}
	case req.PeerID != nil && *req.PeerID != "":
		if p, ok := h.reg.ResolveByIdentifier(*req.PeerID); ok {
			resolved = p
		}
	default:
		if p, ok := h.reg.ResolveByIdentifier(req.Peer); ok {
			resolved = p
		}
	}

	if req.TurnID != nil && *req.TurnID != "" && req.Role == "assistant" {
		markTurnFinalized(*req.TurnID, req.SessionID)
	}

	if resolved != nil && h.store != nil {
		peerID, project := string(resolved.PeerID), resolved.Path
		var sourceURI *string
		for _, key := range []string{"runtime_source_uri", "source_uri", "transcript_source_uri"} {
			if value, _ := resolved.Metadata[key].(string); value != "" {
				sourceURI = &value
				break
			}
		}
		provenance := map[string]any{"source_kind": "runtime_unavailable", "backend": resolved.Backend, "runtime_session_id": req.SessionID, "source_event_id": req.TurnID, "observed_by_peer_id": resolved.PeerID}
		if req.SessionID != nil {
			provenance["source_kind"] = "runtime_transcript"
		}
		_, _ = h.store.UpsertObservation(r.Context(), state.Observation{PeerID: &peerID, Backend: string(resolved.Backend), ProjectPath: &project, RuntimeSessionID: req.SessionID, RuntimeSourceURI: sourceURI, Provenance: provenance, Status: state.BindingActive, Metadata: map[string]any{"last_turn_id": req.TurnID, "last_role": req.Role}})
	}
	if resolved != nil && h.jobCompletion != nil {
		h.jobCompletion.OnChatTurn(r.Context(), resolved.PeerID, req.Role, req.Text)
	}

	h.reg.AddEvent(r.Context(), "chat_turn", data)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// chatTurnDeltaRequest mirrors the Python ChatTurnDeltaRequest. role is always
// "assistant".
type chatTurnDeltaRequest struct {
	Peer       string        `json:"peer"`
	Role       string        `json:"role"`
	SessionID  *string       `json:"session_id,omitempty"`
	TurnID     string        `json:"turn_id"`
	ChunkIndex int           `json:"chunk_index"`
	Kind       string        `json:"kind"`
	Text       string        `json:"text"`
	ToolCall   *toolCallInfo `json:"tool_call,omitempty"`
	IsFinal    bool          `json:"is_final"`
	PeerID     *string       `json:"peer_id,omitempty"`
	PaneID     *string       `json:"pane_id,omitempty"`
}

// ingestChatTurnDelta handles POST /events/chat_delta: a streaming chat-turn
// delta from the per-pane transcript tailer.
//
// Drops deltas whose turn_id already received its final chat_turn (returns 200 so
// the streamer's best-effort post does not retry into a failure loop). Otherwise
// resolves the peer via pane_id (canonicalising "peer") and records via
// reg.AddEvent("chat_turn_delta", data).
func (h *Hub) ingestChatTurnDelta(w http.ResponseWriter, r *http.Request) {
	var req chatTurnDeltaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, "malformed chat delta: "+err.Error())
		return
	}
	if req.Role == "" {
		req.Role = "assistant"
	}
	if req.Role != "assistant" {
		writeJSONError(w, http.StatusUnprocessableEntity, "role must be assistant")
		return
	}
	if req.TurnID == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "turn_id is required")
		return
	}
	if req.ChunkIndex < 0 {
		writeJSONError(w, http.StatusUnprocessableEntity, "chunk_index must be >= 0")
		return
	}
	if req.Kind == "" {
		req.Kind = "text"
	}
	if req.Kind != "text" && req.Kind != "tool_use" {
		writeJSONError(w, http.StatusUnprocessableEntity, "kind must be one of: text, tool_use")
		return
	}

	// Late delta for an already-finalised turn: drop with 200, no retry loop.
	if isTurnFinalized(req.TurnID, req.SessionID) {
		writeJSON(w, http.StatusOK, okResponse{OK: true})
		return
	}

	data := map[string]any{
		"peer":        req.Peer,
		"role":        req.Role,
		"turn_id":     req.TurnID,
		"chunk_index": req.ChunkIndex,
		"kind":        req.Kind,
		"text":        req.Text,
		"is_final":    req.IsFinal,
	}
	if req.SessionID != nil {
		data["session_id"] = *req.SessionID
	}
	if req.ToolCall != nil {
		data["tool_call"] = req.ToolCall
	}
	if req.PeerID != nil {
		data["peer_id"] = *req.PeerID
	}

	if (req.PeerID == nil || *req.PeerID == "") && req.PaneID != nil && *req.PaneID != "" {
		if p, ok := h.reg.GetPeerByPane(*req.PaneID); ok {
			data["peer_id"] = string(p.PeerID)
			data["peer"] = string(p.DisplayName)
		}
	}

	h.reg.AddEvent(r.Context(), "chat_turn_delta", data)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// ---------------------------------------------------------------------------
// Finalised-turn FIFO set.
//
// Bounded set of turn keys that already received their final chat_turn. Late
// chat_turn_delta posts for these are dropped — the dashboard would render them
// as orphan streaming bubbles otherwise. FIFO eviction caps memory growth; the
// keys are message uuids so collisions don't occur in practice. Process-local:
// deltas and finals always pass through the same daemon. Mirrors the Python
// _finalized_turn_ids dict.
// ---------------------------------------------------------------------------

const finalizedTurnIDsCapacity = 4096

var (
	finalizedMu    sync.Mutex
	finalizedKeys  = make(map[string]struct{}, finalizedTurnIDsCapacity)
	finalizedOrder = make([]string, 0, finalizedTurnIDsCapacity) // FIFO order of keys
)

func turnFinalizedKey(sessionID *string, turnID string) string {
	sid := "legacy"
	if sessionID != nil && *sessionID != "" {
		sid = *sessionID
	}
	return sid + ":" + turnID
}

func markTurnFinalized(turnID string, sessionID *string) {
	key := turnFinalizedKey(sessionID, turnID)
	finalizedMu.Lock()
	defer finalizedMu.Unlock()
	if _, seen := finalizedKeys[key]; seen {
		// Refresh recency so a finalised id stays in the set while deltas may
		// still trickle in: move it to the back of the FIFO.
		for i, k := range finalizedOrder {
			if k == key {
				finalizedOrder = append(finalizedOrder[:i], finalizedOrder[i+1:]...)
				break
			}
		}
		finalizedOrder = append(finalizedOrder, key)
		return
	}
	finalizedKeys[key] = struct{}{}
	finalizedOrder = append(finalizedOrder, key)
	for len(finalizedOrder) > finalizedTurnIDsCapacity {
		oldest := finalizedOrder[0]
		finalizedOrder = finalizedOrder[1:]
		delete(finalizedKeys, oldest)
	}
}

func isTurnFinalized(turnID string, sessionID *string) bool {
	key := turnFinalizedKey(sessionID, turnID)
	finalizedMu.Lock()
	defer finalizedMu.Unlock()
	_, seen := finalizedKeys[key]
	return seen
}
