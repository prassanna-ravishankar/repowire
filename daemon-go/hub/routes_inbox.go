package hub

import (
	"context"
	"log"
	"net/http"
	"strconv"

	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Human inbox routes (#466). Every surface (Telegram, the dashboard, the CLI)
// reads and answers the same inbox through these:
//
//   GET  /inbox                  inbound items, newest first (?unread=1, ?limit=N,
//                                ?surface=S pushed there, ?through_ref=N up to that ref)
//   GET  /inbox/{id}             one item with its per-surface push records
//   POST /inbox/{id}/read        mark read everywhere
//   POST /inbox/{id}/reply       answer from a surface
//   POST /inbox/reply            answer by the surface's own message id
//                                (a Telegram reply-to)
//
// A reply to an open ask closes it with the reply (ack, or answer for a
// structured question); a reply to anything else opens a new ask from the
// human to the item's sender. Either way the item is marked replied.
// ============================================================================

func (h *Hub) registerInboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /inbox", h.requireAuth(h.handleInboxList))
	mux.HandleFunc("GET /inbox/{id}", h.requireAuth(h.handleInboxGet))
	mux.HandleFunc("POST /inbox/{id}/read", h.requireAuth(h.handleInboxRead))
	mux.HandleFunc("POST /inbox/{id}/reply", h.requireAuth(h.handleInboxReply))
	mux.HandleFunc("POST /inbox/reply", h.requireAuth(h.handleInboxReply))
}

// InboxReplyRequest is the reply body. Surface names where the human answered
// (telegram, dashboard, cli); SurfaceRef finds the item by the surface's
// message id when the path carries no id; Ref is the surface's id for the
// reply itself, so a follow-up answer can thread to it.
type InboxReplyRequest struct {
	Surface    string  `json:"surface"`
	SurfaceRef string  `json:"surface_ref,omitempty"`
	Ref        string  `json:"ref,omitempty"`
	Text       string  `json:"text"`
	OptionID   *string `json:"option_id,omitempty"`
}

type inboxReadRequest struct {
	Surface string `json:"surface"`
}

func (h *Hub) inboxReady(w http.ResponseWriter) bool {
	if h.inbox == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "human inbox not configured")
		return false
	}
	return true
}

func (h *Hub) handleInboxList(w http.ResponseWriter, r *http.Request) {
	if !h.inboxReady(w) {
		return
	}
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	throughRef, _ := strconv.ParseInt(query.Get("through_ref"), 10, 64)
	unread := query.Get("unread")
	items, err := h.inbox.List(r.Context(), state.InboxFilter{
		UnreadOnly: unread == "1" || unread == "true", Limit: limit,
		Surface: query.Get("surface"), ThroughRef: throughRef,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, inboxItemJSON(&items[i]))
	}
	human, name := h.inbox.Human()
	writeJSON(w, http.StatusOK, map[string]any{"human_peer_id": human, "human": name, "items": out})
}

func (h *Hub) handleInboxGet(w http.ResponseWriter, r *http.Request) {
	if !h.inboxReady(w) {
		return
	}
	item, ok := h.inboxItem(w, r.Context(), r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, inboxItemJSON(item))
}

func (h *Hub) handleInboxRead(w http.ResponseWriter, r *http.Request) {
	if !h.inboxReady(w) {
		return
	}
	var req inboxReadRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	item, ok := h.inboxItem(w, r.Context(), r.PathValue("id"))
	if !ok {
		return
	}
	if err := h.inbox.MarkRead(r.Context(), item, surfaceOr(req.Surface)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item_id": item.ID})
}

func (h *Hub) handleInboxReply(w http.ResponseWriter, r *http.Request) {
	if !h.inboxReady(w) || !h.askReady(w) {
		return
	}
	var req InboxReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	var item *state.InboxItem
	if id := r.PathValue("id"); id != "" {
		var ok bool
		if item, ok = h.inboxItem(w, ctx, id); !ok {
			return
		}
	} else {
		if req.Surface == "" || req.SurfaceRef == "" {
			writeJSONError(w, http.StatusUnprocessableEntity, "surface and surface_ref are required without an item id")
			return
		}
		found, err := h.inbox.BySurfaceRef(ctx, req.Surface, req.SurfaceRef)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found == nil {
			writeJSONError(w, http.StatusNotFound, "no inbox item rendered as "+req.Surface+":"+req.SurfaceRef)
			return
		}
		item = found
	}
	result, err := h.replyToInboxItem(ctx, item, req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// replyToInboxItem is the reply operation every surface shares.
func (h *Hub) replyToInboxItem(ctx context.Context, item *state.InboxItem, req InboxReplyRequest) (map[string]any, error) {
	surface := surfaceOr(req.Surface)
	result := map[string]any{"ok": true, "item_id": item.ID}

	if item.Direction == state.InboxIn && item.Kind == "ask" && item.CorrelationID != "" {
		if ask, ok := h.ask.asks.Get(item.CorrelationID); ok && !ask.Closed {
			if ask.Question != nil {
				answer := AnswerRequest{CorrelationID: item.CorrelationID, OptionID: req.OptionID}
				if req.Text != "" {
					answer.Text = &req.Text
				} else if req.OptionID == nil {
					answer.Outcome = "acknowledged"
				}
				if _, err := h.answerDirect(ctx, answer); err != nil {
					return nil, err
				}
				result["action"] = "answered"
			} else {
				ack := AckRequest{CorrelationID: item.CorrelationID}
				if req.Text != "" {
					ack.Message = &req.Text
				}
				if _, err := h.ackDirect(ctx, ack); err != nil {
					return nil, err
				}
				result["action"] = "acked"
			}
			result["correlation_id"] = item.CorrelationID
			h.markInboxReplied(ctx, item, surface)
			return result, nil
		}
	}

	if req.Text == "" {
		// Nothing to send (an ack button on a closed ask, a "seen" tap).
		result["action"] = "read"
		if err := h.inbox.MarkRead(ctx, item, surface); err != nil {
			return nil, err
		}
		return result, nil
	}
	to := item.FromPeerID
	if item.Direction == state.InboxOut {
		to = item.ToPeerID
	}
	if to == "" {
		return nil, routeErr(http.StatusUnprocessableEntity, "item "+item.ID+" has no sender to reply to")
	}
	human, _ := h.inbox.Human()
	opened, err := h.openAsk(ctx, AskRequest{
		FromPeer: string(human), ToPeer: to, Text: req.Text,
		Origin: &AskOrigin{Surface: surface, Ref: req.Ref},
	})
	if err != nil {
		return nil, err
	}
	result["action"] = "asked"
	result["correlation_id"] = opened.CorrelationID
	h.markInboxReplied(ctx, item, surface)
	return result, nil
}

// markInboxReplied records replied state after the reply itself went out. The
// reply already reached its recipient, so a failed bookkeeping write is logged
// rather than reported as a failed reply (which would invite a resend).
func (h *Hub) markInboxReplied(ctx context.Context, item *state.InboxItem, surface string) {
	if err := h.inbox.MarkReplied(ctx, item, surface); err != nil {
		log.Printf("inbox: mark %s replied: %v", item.ID, err)
	}
}

func (h *Hub) inboxItem(w http.ResponseWriter, ctx context.Context, id string) (*state.InboxItem, bool) {
	item, err := h.inbox.Get(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if item == nil {
		writeJSONError(w, http.StatusNotFound, "no inbox item "+id)
		return nil, false
	}
	return item, true
}

func surfaceOr(surface string) string {
	if surface == "" {
		return "api"
	}
	return surface
}

func inboxItemJSON(item *state.InboxItem) map[string]any {
	pushes := make([]map[string]any, 0, len(item.Pushes))
	for _, p := range item.Pushes {
		push := map[string]any{"surface": p.Surface, "status": p.Status, "ts": p.TS}
		if p.SurfaceRef != "" {
			push["surface_ref"] = p.SurfaceRef
		}
		if p.ErrorClass != "" {
			push["error_class"] = p.ErrorClass
		}
		pushes = append(pushes, push)
	}
	out := map[string]any{
		"id": item.ID, "direction": item.Direction, "kind": item.Kind,
		"from": item.FromName, "from_peer_id": item.FromPeerID,
		"to": item.ToName, "to_peer_id": item.ToPeerID,
		"text": item.Text, "created_at": item.CreatedAt,
		"read": item.ReadAt != "", "replied": item.RepliedAt != "",
		"pushes": pushes,
	}
	optional := map[string]string{
		"correlation_id": item.CorrelationID, "read_at": item.ReadAt, "read_surface": item.ReadSurface,
		"replied_at": item.RepliedAt, "reply_surface": item.ReplySurface,
	}
	for k, v := range optional {
		if v != "" {
			out[k] = v
		}
	}
	if len(item.Attachments) > 0 {
		out["attachments"] = item.Attachments
	}
	if item.Question != nil {
		out["question"] = item.Question
	}
	return out
}
