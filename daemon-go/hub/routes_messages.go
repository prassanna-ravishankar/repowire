package hub

import (
	"context"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Agent peer messages (rfcs/message-ids-pull-delegate.md). Every message an
// agent peer receives has an id; these routes list, peek at and fetch them,
// and set the peer's delivery mode:
//
//   GET  /messages?peer=P            P's messages, newest first (?unread=1, ?limit=N)
//   GET  /messages/{id}              one message, left unread (peek); ?peer=P
//                                    answers only if P is its recipient
//   POST /messages/{id}/read         one message, marked read (fetch); ?peer=P too
//   POST /peers/{name}/message-delivery   {"mode": "push" | "pull"}
//
// The human's messages are its inbox (/inbox), not these.
// ============================================================================

// WithMessages wires the agent message record behind /messages.
func (h *Hub) WithMessages(messages *service.Messages) *Hub {
	h.messages = messages
	return h
}

func (h *Hub) registerMessageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /messages", h.requireAuth(h.handleMessageList))
	mux.HandleFunc("GET /messages/{id}", h.requireAuth(h.handleMessageGet))
	mux.HandleFunc("POST /messages/{id}/read", h.requireAuth(h.handleMessageGet))
	mux.HandleFunc("POST /peers/{name}/message-delivery", h.requireAuth(h.handleMessageDelivery))
}

// MessageView is one message on the wire. Text is set only when the message
// itself was asked for; a listing carries the preview.
type MessageView struct {
	MessageID     string           `json:"message_id"`
	Kind          string           `json:"kind"`
	From          string           `json:"from"`
	FromPeerID    string           `json:"from_peer_id,omitempty"`
	FromRole      string           `json:"from_role,omitempty"` // "human" when the human sent it
	To            string           `json:"to"`
	ToPeerID      string           `json:"to_peer_id"`
	CorrelationID string           `json:"correlation_id,omitempty"`
	Preview       string           `json:"preview"`
	Chars         int              `json:"chars"`
	Text          *string          `json:"text,omitempty"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
	Question      map[string]any   `json:"question,omitempty"`
	CreatedAt     string           `json:"created_at"`
	ReadAt        *string          `json:"read_at"`
}

// MessageListResponse is one peer's messages and its delivery mode.
type MessageListResponse struct {
	Peer     string        `json:"peer"`
	PeerID   string        `json:"peer_id"`
	Delivery string        `json:"delivery"`
	Messages []MessageView `json:"messages"`
}

func (h *Hub) messageView(msg *state.PeerMessage, withBody bool) MessageView {
	view := MessageView{
		MessageID: msg.ID, Kind: msg.Kind, From: msg.FromName, FromPeerID: msg.FromPeerID,
		To: msg.PeerName, ToPeerID: msg.PeerID, CorrelationID: msg.CorrelationID,
		Preview: service.MessagePreview(msg.Text), Chars: utf8.RuneCountInString(msg.Text),
		CreatedAt: msg.CreatedAt, ReadAt: strPtr(msg.ReadAt),
	}
	if human, _ := h.inbox.Human(); human != "" && string(human) == msg.FromPeerID {
		view.FromRole = string(proto.RoleHuman)
	}
	if withBody {
		view.Text, view.Attachments, view.Question = &msg.Text, msg.Attachments, msg.Question
	}
	return view
}

func (h *Hub) messagesReady() error {
	if h.messages == nil {
		return routeErr(http.StatusServiceUnavailable, "message store not configured")
	}
	return nil
}

func (h *Hub) handleMessageList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	result, err := h.listMessages(r.Context(), q.Get("peer"), q.Get("unread") == "1" || q.Get("unread") == "true", limit)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Hub) handleMessageGet(w http.ResponseWriter, r *http.Request) {
	var reader proto.PeerID
	if scope := r.URL.Query().Get("peer"); scope != "" {
		p, err := h.messagePeer(scope)
		if err != nil {
			writeRouteError(w, err)
			return
		}
		reader = p.PeerID
	}
	view, err := h.readMessage(r.Context(), r.PathValue("id"), reader, r.Method == http.MethodGet)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Hub) handleMessageDelivery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := h.setMessageDelivery(r.Context(), r.PathValue("name"), req.Mode)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer": string(p.DisplayName), "peer_id": string(p.PeerID), "delivery": req.Mode})
}

// messagePeer resolves the peer whose messages are asked for.
func (h *Hub) messagePeer(identifier string) (*proto.Peer, error) {
	if identifier == "" {
		return nil, routeErr(http.StatusUnprocessableEntity, "peer is required")
	}
	p, err := h.reg.GetPeerByName(identifier, nil)
	if err != nil {
		return nil, routeErr(http.StatusConflict, err.Error())
	}
	if p == nil {
		return nil, routeErr(http.StatusNotFound, "Unknown peer: "+identifier)
	}
	return p, nil
}

// listMessages is the typed list operation shared by HTTP and MCP callers.
func (h *Hub) listMessages(ctx context.Context, identifier string, unreadOnly bool, limit int) (MessageListResponse, error) {
	if err := h.messagesReady(); err != nil {
		return MessageListResponse{}, err
	}
	p, err := h.messagePeer(identifier)
	if err != nil {
		return MessageListResponse{}, err
	}
	msgs, err := h.messages.List(ctx, state.PeerMessageFilter{PeerID: string(p.PeerID), UnreadOnly: unreadOnly, Limit: limit})
	if err != nil {
		return MessageListResponse{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	out := MessageListResponse{Peer: string(p.DisplayName), PeerID: string(p.PeerID), Delivery: h.messages.Mode(ctx, p.PeerID), Messages: make([]MessageView, 0, len(msgs))}
	for i := range msgs {
		out.Messages = append(out.Messages, h.messageView(&msgs[i], false))
	}
	return out, nil
}

// readMessage returns one message with its body; unless peek, it is marked
// read. reader, when set, must be the message's recipient: anyone else is
// told it does not exist.
func (h *Hub) readMessage(ctx context.Context, id string, reader proto.PeerID, peek bool) (MessageView, error) {
	if err := h.messagesReady(); err != nil {
		return MessageView{}, err
	}
	msg, err := h.messages.Get(ctx, id)
	if err != nil {
		return MessageView{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	if msg == nil || (reader != "" && proto.PeerID(msg.PeerID) != reader) {
		return MessageView{}, routeErr(http.StatusNotFound, "No message with id: "+id)
	}
	if !peek && msg.ReadAt == "" {
		h.messages.MarkRead(ctx, msg)
		if fresh, err := h.messages.Get(ctx, id); err == nil && fresh != nil {
			msg = fresh
		}
	}
	return h.messageView(msg, true), nil
}

// setMessageDelivery records a peer's delivery mode.
func (h *Hub) setMessageDelivery(ctx context.Context, identifier, mode string) (*proto.Peer, error) {
	if err := h.messagesReady(); err != nil {
		return nil, err
	}
	p, err := h.messagePeer(identifier)
	if err != nil {
		return nil, err
	}
	if err := h.messages.SetMode(ctx, p.PeerID, mode); err != nil {
		return nil, routeErr(http.StatusUnprocessableEntity, err.Error())
	}
	return p, nil
}

// formatMessage renders a fetched message for an agent with the same
// human/peer framing its push would have had.
func formatMessage(view MessageView) string {
	body := ""
	if view.Text != nil {
		body = *view.Text
	}
	return hooks.FormatInboundMessage(view.From, view.FromRole, view.To, view.Kind, view.CorrelationID, view.MessageID, body+hooks.FormatAttachments(attachmentsAny(view.Attachments)))
}

func attachmentsAny(attachments []map[string]any) any {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]any, len(attachments))
	for i, a := range attachments {
		out[i] = a
	}
	return out
}
