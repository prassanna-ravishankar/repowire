package hub

// POST /asks/{correlation_id}/delegate: hand an open ask to another peer
// (rfcs/message-ids-pull-delegate.md). The correlation id, the asker and the
// reply path are unchanged; only the recipient moves, and the hop is recorded.
// The whole hand-off (checks, retarget, delivery, rollback) runs under the
// ask's Claim, so no reply, answer, completion or other delegation can
// interleave with it, and a delivery that fails returns the ask to the
// delegator.
//
// The new recipient receives the ask from the delegator, not the asker: the
// delegator wrote the note and chose to pass the body on, so a peer can never
// present its words with the asker's (possibly the human's) authority.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// DelegateRequest is the /asks/{cid}/delegate body. FromPeer names the caller;
// when it resolves to a registered peer, that peer must hold the ask.
type DelegateRequest struct {
	ToPeer   string  `json:"to_peer"`
	Note     string  `json:"note,omitempty"`
	FromPeer *string `json:"from_peer,omitempty"`
	Circle   *string `json:"circle,omitempty"`
}

// DelegateResponse reports the new recipient and the whole chain.
type DelegateResponse struct {
	CorrelationID string           `json:"correlation_id"`
	DelegatedTo   string           `json:"delegated_to"`
	Transport     string           `json:"transport"`
	MessageID     string           `json:"message_id,omitempty"`
	Delegations   []DelegationView `json:"delegations"`
}

func (h *Hub) handleDelegate(w http.ResponseWriter, r *http.Request) {
	if !h.askReady(w) {
		return
	}
	var req DelegateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.delegateAsk(r.Context(), r.PathValue("correlation_id"), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// delegateAsk is the typed delegate operation shared by HTTP and MCP callers.
func (h *Hub) delegateAsk(ctx context.Context, cid string, req DelegateRequest) (DelegateResponse, error) {
	if err := h.askOperationReady(); err != nil {
		return DelegateResponse{}, err
	}
	if strings.TrimSpace(req.ToPeer) == "" {
		return DelegateResponse{}, routeErr(http.StatusUnprocessableEntity, "to_peer is required")
	}
	ask, ok := h.ask.asks.Get(cid)
	if !ok {
		return DelegateResponse{}, routeErr(http.StatusNotFound, "No open ask with correlation_id: "+cid)
	}
	if ask.Closed {
		return DelegateResponse{}, routeErr(http.StatusGone, fmt.Sprintf("Ask %s is already closed", cid))
	}
	if proto.IsRemotePeerID(ask.FromPeerID) || proto.IsRemotePeerID(ask.ToPeerID) {
		return DelegateResponse{}, routeErr(http.StatusUnprocessableEntity, "a cross-machine ask cannot be delegated")
	}
	if _, _, err := h.remoteAddress(ctx, req.ToPeer); err == nil {
		return DelegateResponse{}, routeErr(http.StatusUnprocessableEntity, "an ask cannot be delegated to another machine")
	}
	if work := h.jobForAsk(ctx, cid); work != "" {
		return DelegateResponse{}, routeErr(http.StatusConflict, map[string]any{
			"error": "job_ask", "correlation_id": cid, "job_id": work,
			"hint": "This ask dispatches a job; its completion follows the assigned peer's turn, so it cannot be delegated.",
		})
	}
	var by proto.PeerID
	if req.FromPeer != nil {
		if caller, _ := h.ask.reg.GetPeerByName(*req.FromPeer, nil); caller != nil {
			by = caller.PeerID
		}
	}

	ask, err := h.ask.asks.Claim(cid)
	if err != nil {
		return DelegateResponse{}, resolveClaimErr(cid, err)
	}
	defer h.ask.asks.Release(cid)
	if by != "" && by != ask.ToPeerID {
		if ask.HeldBefore(by) {
			return DelegateResponse{}, routeErr(http.StatusConflict, map[string]any{
				"error": "ask_delegated", "correlation_id": cid, "holder": string(ask.ToPeerName),
			})
		}
		return DelegateResponse{}, delegateErr(cid, ask, service.ErrNotHolder)
	}
	target, err := h.delegateTarget(ctx, ask, req)
	if err != nil {
		return DelegateResponse{}, err
	}
	moved, hop, err := h.ask.asks.Delegate(ctx, cid, service.DelegateParams{
		By: by, To: target.PeerID, ToName: target.DisplayName,
		ToSessionID: h.sessionIDForPeer(ctx, string(target.PeerID)), Note: strings.TrimSpace(req.Note),
	})
	if err != nil {
		return DelegateResponse{}, delegateErr(cid, ask, err)
	}
	h.traceAsk(ctx, cid, "delegated", "", moved, map[string]any{
		"from_peer_id": string(hop.FromPeerID), "from": string(hop.FromName),
		"to_peer_id": string(hop.ToPeerID), "to": string(hop.ToName),
		"note": clipTraceText(hop.Note), "hops": len(moved.Delegations),
	})

	delivered, err := h.ask.delivery.DeliverAsk(ctx, service.DeliverAskParams{
		FromPeer:      string(hop.FromPeerID),
		ToPeer:        string(target.PeerID),
		Text:          delegatedText(hop, moved),
		CorrelationID: cid,
		BypassCircle:  true, // the delegator was authorized against the target above
		Attachments:   moved.Attachments,
		Question:      moved.Question,
	})
	if err != nil {
		h.traceAsk(ctx, cid, "delegate_failed", "fail", ask, map[string]any{"to_peer_id": string(target.PeerID), "error": err.Error()})
		if !h.ask.asks.UndoDelegation(ctx, cid, hop) {
			// Only a path that does not claim (eviction, a demotion closing
			// the target's asks) can get here: the ask is no longer open.
			return DelegateResponse{}, routeErr(http.StatusGone, fmt.Sprintf(
				"Delivery to %s failed (%s) and ask %s was closed meanwhile.", hop.ToName, err, cid))
		}
		return DelegateResponse{}, delegateDeliveryErr(cid, hop, err)
	}
	recordDeliveryOutcome(ctx, h.deliveryTraces, cid, "ask", string(target.PeerID), string(moved.FromPeerID), delivered.Transport, delivered.HookDelivery)
	h.ask.reg.AddEvent(ctx, "ask_delegated", map[string]any{
		"correlation_id": cid,
		"from":           string(hop.FromName), "from_peer_id": string(hop.FromPeerID),
		"to": string(hop.ToName), "to_peer_id": string(hop.ToPeerID),
		"asker": string(moved.FromPeerName), "asker_peer_id": string(moved.FromPeerID),
		"note": hop.Note, "hops": len(moved.Delegations),
	})
	return DelegateResponse{
		CorrelationID: cid,
		DelegatedTo:   string(target.DisplayName),
		Transport:     delivered.Transport,
		MessageID:     delivered.MessageID,
		Delegations:   delegationViews(moved.Delegations),
	}, nil
}

// delegateTarget resolves the new recipient and authorizes the current one
// to reach it, under the ordinary circle and addressability rules.
func (h *Hub) delegateTarget(ctx context.Context, ask *service.Ask, req DelegateRequest) (*proto.Peer, error) {
	target, err := h.ask.reg.GetPeerByName(req.ToPeer, req.Circle)
	if err != nil {
		return nil, routeErr(http.StatusConflict, err.Error())
	}
	if target == nil {
		return nil, routeErr(http.StatusNotFound, "Unknown peer: "+req.ToPeer)
	}
	holder, authorized, err := h.ask.reg.CheckAccess(ctx, string(ask.ToPeerID), string(target.PeerID), false, nil)
	if err != nil {
		if na := h.notAddressableRouteErr(err); na != nil {
			return nil, na
		}
		return nil, routeErr(http.StatusForbidden, err.Error())
	}
	if na := h.notAddressableRouteErr(proto.RequireAddressable(holder, authorized)); na != nil {
		return nil, na
	}
	switch authorized.PeerID {
	case ask.FromPeerID:
		return nil, routeErr(http.StatusUnprocessableEntity, "cannot delegate an ask to its asker; decline it instead")
	case ask.ToPeerID:
		return nil, routeErr(http.StatusUnprocessableEntity, "the ask is already with "+string(ask.ToPeerName))
	}
	if ask.HeldBefore(authorized.PeerID) {
		return nil, delegateErr(ask.CorrelationID, ask, service.ErrFormerHolder)
	}
	return authorized, nil
}

// jobForAsk names the live job whose dispatch ask cid is, or "".
func (h *Hub) jobForAsk(ctx context.Context, cid string) string {
	if h.store == nil {
		return ""
	}
	works, err := h.store.ListWork(ctx, state.WorkFilter{CorrelationID: &cid})
	if err != nil {
		return ""
	}
	for _, work := range works {
		if !work.Terminal() {
			return work.WorkID
		}
	}
	return ""
}

// delegatedText is the ask as the new recipient sees it, sent by the
// delegator: who handed it on, whose question it is, why, and the question.
func delegatedText(hop service.Delegation, ask *service.Ask) string {
	head := "[delegated by @" + string(hop.FromName) + " for @" + string(ask.FromPeerName)
	if hop.Note != "" {
		head += ": " + hop.Note
	}
	return head + "]\n" + ask.Text
}

func delegateErr(cid string, ask *service.Ask, err error) error {
	switch {
	case errors.Is(err, service.ErrAskNotFound):
		return routeErr(http.StatusNotFound, "No open ask with correlation_id: "+cid)
	case errors.Is(err, service.ErrAlreadyAnswered):
		return routeErr(http.StatusGone, fmt.Sprintf("Ask %s is already closed", cid))
	case errors.Is(err, service.ErrNotHolder):
		return routeErr(http.StatusForbidden, map[string]any{
			"error": "not_the_recipient", "correlation_id": cid, "holder": string(ask.ToPeerName),
		})
	case errors.Is(err, service.ErrAskResolving):
		return routeErr(http.StatusConflict, fmt.Sprintf("Ask %s is being resolved; it was not delegated", cid))
	case errors.Is(err, service.ErrQuiesced):
		return routeErr(http.StatusConflict, map[string]any{"error": "peer_switching", "hint": "The target is mid-switch; retry shortly."})
	case errors.Is(err, service.ErrResultWaiting):
		return resolveClaimErr(cid, err)
	case errors.Is(err, service.ErrFormerHolder):
		return routeErr(http.StatusUnprocessableEntity, map[string]any{
			"error": "former_holder", "correlation_id": cid,
			"hint": "That peer held this ask before; an ask never returns to a former holder. Pick another peer, or answer it yourself.",
		})
	case errors.Is(err, service.ErrDelegationLimit):
		return routeErr(http.StatusUnprocessableEntity, fmt.Sprintf("Ask %s has been delegated %d times; answer or decline it", cid, service.MaxDelegations))
	default:
		return routeErr(http.StatusUnprocessableEntity, err.Error())
	}
}

// delegateDeliveryErr reports a delivery that failed after the hop was undone:
// the ask is back with the delegator.
func delegateDeliveryErr(cid string, hop service.Delegation, err error) error {
	if di, ok := service.AsDeliveryInjection(err); ok {
		return routeErr(http.StatusServiceUnavailable, map[string]any{
			"error": "injection_failed", "correlation_id": cid,
			"hint": fmt.Sprintf("Delivery to %s failed (%s); the ask stays with you.", hop.ToName, di.Error()),
		})
	}
	if errors.Is(err, service.ErrNotConnected) || errors.Is(err, service.ErrTransportUnavailable) {
		return routeErr(http.StatusServiceUnavailable, fmt.Sprintf(
			"Peer %s has no live connection: %s. The ask stays with you.", hop.ToName, err))
	}
	return routeErr(http.StatusNotFound, err.Error()+"; the ask stays with you")
}
