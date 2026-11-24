package hub

// Wire actions (rfcs/wire-actions.md): reassign, nudge and cancel an open
// ask, for the operator. They live beside delegate in the ask lifecycle and
// share its claim, move, deliver and undo path; the wire routes and the CLI
// only reach them.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

const (
	// operatorSender is who the parties hear an action from. ':' is outside
	// the peer-name pattern (validNameRe), so it never resolves to a peer.
	operatorSender  = "repowire:operator"
	operatorActor   = "operator"
	maxActionReason = 200
	nudgeEvery      = 10 * time.Minute
)

// WireActionResult reports an action: the ask, its holder afterwards, what
// each notified party's notice did, and how many queued copies were dropped.
type WireActionResult struct {
	CorrelationID string            `json:"correlation_id"`
	Action        string            `json:"action"`
	Holder        string            `json:"holder,omitempty"`
	Notified      map[string]string `json:"notified,omitempty"`
	DroppedQueued int               `json:"dropped_queued,omitempty"`
}

func actionText(field, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case required && value == "":
		return "", routeErr(http.StatusUnprocessableEntity, field+" is required")
	case len([]rune(value)) > maxActionReason:
		return "", routeErr(http.StatusUnprocessableEntity, fmt.Sprintf("%s is at most %d characters", field, maxActionReason))
	}
	return value, nil
}

// actionableAsk checks what every action refuses: an unknown or closed ask,
// one with a remote end, and a job's dispatch ask.
func (h *Hub) actionableAsk(ctx context.Context, cid string) (*service.Ask, error) {
	if err := h.askOperationReady(); err != nil {
		return nil, err
	}
	ask, ok := h.ask.asks.Get(cid)
	if !ok {
		return nil, routeErr(http.StatusNotFound, "No open ask with correlation_id: "+cid)
	}
	if ask.Closed {
		return nil, routeErr(http.StatusGone, fmt.Sprintf("Ask %s is already closed", cid))
	}
	if proto.IsRemotePeerID(ask.FromPeerID) || proto.IsRemotePeerID(ask.ToPeerID) {
		return nil, routeErr(http.StatusUnprocessableEntity, "a cross-machine ask cannot be acted on here")
	}
	if work := h.jobForAsk(ctx, cid); work != "" {
		return nil, routeErr(http.StatusConflict, map[string]any{
			"error": "job_ask", "correlation_id": cid, "job_id": work,
			"hint": "This ask dispatches a job; use the job tools.",
		})
	}
	return ask, nil
}

// operatorNotice tells a party about an action, best effort, and reports
// what happened to the notice.
func (h *Hub) operatorNotice(ctx context.Context, to proto.PeerID, text string) string {
	res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{FromPeer: operatorSender, ToPeer: string(to), Text: text, BypassCircle: true})
	switch {
	case err != nil:
		return "failed: " + err.Error()
	case res.Delivered():
		return "delivered"
	case res.Queued():
		return "queued"
	}
	return "failed: " + res.Reason
}

// dropQueuedCopies removes what the delivery queue still holds for a former
// holder about cid. An ACP prompt already in flight cannot be withdrawn; its
// completion carries an older generation and is dropped.
func (h *Hub) dropQueuedCopies(ctx context.Context, cid string, holder proto.PeerID, reason string) int {
	if h.store == nil {
		return 0
	}
	n, err := h.store.DropQueuedForAsk(ctx, cid, string(holder), reason)
	if err != nil {
		h.ask.reg.AddEvent(ctx, "wire_action_error", map[string]any{"correlation_id": cid, "error": err.Error()})
	}
	return n
}

// reassignAsk moves an open ask to another peer on the operator's authority.
func (h *Hub) reassignAsk(ctx context.Context, cid, to, reason string) (WireActionResult, error) {
	reason, err := actionText("reason", reason, true)
	if err != nil {
		return WireActionResult{}, err
	}
	ask, err := h.actionableAsk(ctx, cid)
	if err != nil {
		return WireActionResult{}, err
	}
	if _, _, err := h.remoteAddress(ctx, to); err == nil {
		return WireActionResult{}, routeErr(http.StatusUnprocessableEntity, "an ask cannot be moved to another machine")
	}
	target, err := h.ask.reg.GetPeerByName(to, nil)
	if err != nil {
		return WireActionResult{}, routeErr(http.StatusConflict, err.Error())
	}
	if target == nil {
		return WireActionResult{}, routeErr(http.StatusNotFound, "Unknown peer: "+to)
	}
	if na := h.notAddressableRouteErr(proto.RequireAddressable(nil, target)); na != nil {
		return WireActionResult{}, na
	}

	claimed, err := h.ask.asks.Claim(cid)
	if err != nil {
		return WireActionResult{}, resolveClaimErr(cid, err)
	}
	defer h.ask.asks.Release(cid)
	moved, hop, err := h.ask.asks.Reassign(ctx, cid, service.ReassignParams{
		By: operatorActor, To: target.PeerID, ToName: target.DisplayName,
		ToSessionID: h.sessionIDForPeer(ctx, string(target.PeerID)), Reason: reason,
	})
	if err != nil {
		return WireActionResult{}, delegateErr(cid, ask, err)
	}
	h.traceAsk(ctx, cid, "reassigned", "", moved, map[string]any{
		"by": operatorActor, "from_peer_id": string(hop.FromPeerID), "to_peer_id": string(hop.ToPeerID),
		"reason": clipTraceText(reason), "hops": len(moved.Delegations),
	})
	text := fmt.Sprintf("[reassigned by the operator from @%s for @%s: %s]\n%s", hop.FromName, moved.FromPeerName, reason, moved.Text)
	if _, err := h.deliverHop(ctx, claimed, moved, hop, operatorSender, text); err != nil {
		return WireActionResult{}, err
	}

	// Both notices go out only once the new holder accepted the ask.
	result := WireActionResult{CorrelationID: cid, Action: "reassign", Holder: string(hop.ToName), Notified: map[string]string{}}
	result.DroppedQueued = h.dropQueuedCopies(ctx, cid, hop.FromPeerID, "reassigned")
	result.Notified["@"+string(hop.FromName)] = h.operatorNotice(ctx, hop.FromPeerID, fmt.Sprintf(
		"[ask #%s moved to @%s by the operator: %s] A reply from you to this ask will be refused.", cid, hop.ToName, reason))
	result.Notified["@"+string(moved.FromPeerName)] = h.operatorNotice(ctx, moved.FromPeerID, fmt.Sprintf(
		"[your ask #%s moved from @%s to @%s by the operator: %s]", cid, hop.FromName, hop.ToName, reason))
	h.ask.reg.AddEvent(ctx, "ask_reassigned", map[string]any{
		"correlation_id": cid, "actor": operatorActor,
		"from": string(hop.FromName), "from_peer_id": string(hop.FromPeerID),
		"to": string(hop.ToName), "to_peer_id": string(hop.ToPeerID),
		"asker": string(moved.FromPeerName), "asker_peer_id": string(moved.FromPeerID),
		"reason": reason, "hops": len(moved.Delegations),
	})
	return result, nil
}

// cancelAsk closes an open ask on the operator's authority. The asker gets
// the cancellation as the ask's outcome on its reply path; the holder is told
// to stop.
func (h *Hub) cancelAsk(ctx context.Context, cid, reason string) (WireActionResult, error) {
	reason, err := actionText("reason", reason, true)
	if err != nil {
		return WireActionResult{}, err
	}
	if _, err := h.actionableAsk(ctx, cid); err != nil {
		return WireActionResult{}, err
	}
	claimed, err := h.ask.asks.Claim(cid)
	if err != nil {
		return WireActionResult{}, resolveClaimErr(cid, err)
	}
	defer h.ask.asks.Release(cid)
	if claimed.PendingReply != nil {
		// The stash's redelivery runs outside the claim: a cancel could race
		// it into both a reply and a cancellation.
		return WireActionResult{}, resolveClaimErr(cid, service.ErrResultWaiting)
	}
	holder, holderName := claimed.ToPeerID, claimed.ToPeerName
	framed := fmt.Sprintf("[ask #%s cancelled by the operator] %s", cid, reason)
	if err := h.deliverResolution(ctx, claimed, operatorSender, framed, "Cancelled: "+reason, "cancelled", reason, nil); err != nil {
		return WireActionResult{}, err
	}

	result := WireActionResult{CorrelationID: cid, Action: "cancel", Notified: map[string]string{}}
	result.DroppedQueued = h.dropQueuedCopies(ctx, cid, holder, "cancelled")
	result.Notified["@"+string(holderName)] = h.operatorNotice(ctx, holder, fmt.Sprintf(
		"[ask #%s from @%s cancelled by the operator: %s] Stop work on it; a reply will be refused.", cid, claimed.FromPeerName, reason))
	h.ask.reg.AddEvent(ctx, "ask_cancelled", map[string]any{
		"correlation_id": cid, "actor": operatorActor, "holder": string(holderName), "holder_peer_id": string(holder),
		"asker": string(claimed.FromPeerName), "asker_peer_id": string(claimed.FromPeerID), "reason": reason,
	})
	return result, nil
}

// nudgeAsk reminds an open ask's holder, at most once per nudgeEvery, and
// records the nudge on the ask's trace.
func (h *Hub) nudgeAsk(ctx context.Context, cid, note string) (WireActionResult, error) {
	note, err := actionText("note", note, false)
	if err != nil {
		return WireActionResult{}, err
	}
	ask, err := h.actionableAsk(ctx, cid)
	if err != nil {
		return WireActionResult{}, err
	}
	if h.store != nil {
		rows, err := h.store.StagesFor(ctx, cid)
		if err != nil {
			return WireActionResult{}, err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Stage != "nudged" {
				continue
			}
			if at, perr := time.Parse(time.RFC3339Nano, rows[i].TS); perr == nil && time.Since(at) < nudgeEvery {
				return WireActionResult{}, routeErr(http.StatusTooManyRequests, map[string]any{
					"error": "nudged_recently", "correlation_id": cid,
					"hint": fmt.Sprintf("Nudged %s ago; one nudge per %s.", time.Since(at).Round(time.Second), nudgeEvery),
				})
			}
			break
		}
	}
	text := fmt.Sprintf("[nudge from the operator: ask #%s from @%s, open %s]", cid, ask.FromPeerName, time.Since(ask.CreatedAt).Round(time.Minute))
	if note != "" {
		text = strings.TrimSuffix(text, "]") + ": " + note + "]"
	}
	result := WireActionResult{CorrelationID: cid, Action: "nudge", Holder: string(ask.ToPeerName), Notified: map[string]string{}}
	result.Notified["@"+string(ask.ToPeerName)] = h.operatorNotice(ctx, ask.ToPeerID, text)
	h.traceAsk(ctx, cid, "nudged", "", ask, map[string]any{"by": operatorActor, "note": clipTraceText(note)})
	return result, nil
}
