package hub

// Wire actions (rfcs/wire-actions.md): reassign, nudge and cancel an open
// ask, for the operator and for orchestrators. They live beside delegate in
// the ask lifecycle and share its claim, move, deliver and undo path; the
// wire routes, the CLI and the MCP tools only reach them.

import (
	"context"
	"errors"
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

// wireActor is who takes an action: the operator (peer nil), or a proven
// orchestrator, whom the thread's records must admit.
type wireActor struct {
	peer *proto.Peer
}

var operator = wireActor{}

// by is the actor as the hop, the trace and the event record it.
func (a wireActor) by() string {
	if a.peer == nil {
		return operatorActor
	}
	return string(a.peer.PeerID)
}

// sender is who the parties hear the action from.
func (a wireActor) sender() string {
	if a.peer == nil {
		return operatorSender
	}
	return string(a.peer.PeerID)
}

// label names the actor in the parties' notices.
func (a wireActor) label() string {
	if a.peer == nil {
		return "the operator"
	}
	return "@" + string(a.peer.DisplayName)
}

// authorize admits the operator, and an orchestrator only where the wire view
// allows it (service.Wire.OrchestratorMay). dest is a reassign's new holder.
// Callers hold the ask's claim.
func (h *Hub) authorize(ctx context.Context, a wireActor, cid string, dest *proto.Peer) error {
	if a.peer == nil {
		return nil
	}
	err := h.wire.AuthorizeOrchestrator(ctx, cid, a.peer.Circle, dest)
	var refused service.WireRefusal
	if errors.As(err, &refused) {
		return routeErr(http.StatusForbidden, map[string]any{"error": "wire_refused", "correlation_id": cid, "reason": refused.Reason})
	}
	return err
}

// wireAction runs action on the thread id as the wire view shows it.
func (h *Hub) wireAction(ctx context.Context, a wireActor, action, threadID string, req WireActionRequest) (WireActionResult, error) {
	cid, ok, err := h.wire.Resolve(ctx, threadID)
	if err != nil {
		return WireActionResult{}, err
	}
	if !ok {
		return WireActionResult{}, routeErr(http.StatusNotFound, "No thread: "+threadID)
	}
	switch action {
	case "reassign":
		return h.reassignAsk(ctx, a, cid, req.To, req.Reason)
	case "cancel":
		return h.cancelAsk(ctx, a, cid, req.Reason)
	}
	return h.nudgeAsk(ctx, a, cid, req.Note)
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

// actionNotice tells a party about an action, best effort, and reports what
// happened to the notice.
func (h *Hub) actionNotice(ctx context.Context, a wireActor, to proto.PeerID, text string) string {
	res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{FromPeer: a.sender(), ToPeer: string(to), Text: text, BypassCircle: true})
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

// reassignAsk moves an open ask to another peer on a's authority.
func (h *Hub) reassignAsk(ctx context.Context, a wireActor, cid, to, reason string) (WireActionResult, error) {
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
	if err := h.authorize(ctx, a, cid, target); err != nil {
		return WireActionResult{}, err
	}
	moved, hop, err := h.ask.asks.Reassign(ctx, cid, service.ReassignParams{
		By: a.by(), To: target.PeerID, ToName: target.DisplayName,
		ToSessionID: h.sessionIDForPeer(ctx, string(target.PeerID)), Reason: reason,
	})
	if err != nil {
		return WireActionResult{}, delegateErr(cid, ask, err)
	}
	// The acting orchestrator is recorded as a party: the thread passed
	// through its circle. The operator is not a peer.
	var actorID proto.PeerID
	if a.peer != nil {
		actorID = a.peer.PeerID
	}
	h.traceAsk(ctx, cid, "reassigned", "", moved, h.wire.WithProvenance(ctx, map[string]any{
		"by": a.by(), "from_peer_id": string(hop.FromPeerID), "to_peer_id": string(hop.ToPeerID),
		"reason": clipTraceText(reason), "hops": len(moved.Delegations),
	}, hop.ToPeerID, actorID))
	text := fmt.Sprintf("[reassigned by %s from @%s for @%s: %s]\n%s", a.label(), hop.FromName, moved.FromPeerName, reason, moved.Text)
	if _, err := h.deliverHop(ctx, claimed, moved, hop, a.sender(), text); err != nil {
		return WireActionResult{}, err
	}

	// Both notices go out only once the new holder accepted the ask.
	result := WireActionResult{CorrelationID: cid, Action: "reassign", Holder: string(hop.ToName), Notified: map[string]string{}}
	result.DroppedQueued = h.dropQueuedCopies(ctx, cid, hop.FromPeerID, "reassigned")
	result.Notified["@"+string(hop.FromName)] = h.actionNotice(ctx, a, hop.FromPeerID, fmt.Sprintf(
		"[ask #%s moved to @%s by %s: %s] A reply from you to this ask will be refused.", cid, hop.ToName, a.label(), reason))
	result.Notified["@"+string(moved.FromPeerName)] = h.actionNotice(ctx, a, moved.FromPeerID, fmt.Sprintf(
		"[your ask #%s moved from @%s to @%s by %s: %s]", cid, hop.FromName, hop.ToName, a.label(), reason))
	h.ask.reg.AddEvent(ctx, "ask_reassigned", map[string]any{
		"correlation_id": cid, "actor": a.by(),
		"from": string(hop.FromName), "from_peer_id": string(hop.FromPeerID),
		"to": string(hop.ToName), "to_peer_id": string(hop.ToPeerID),
		"asker": string(moved.FromPeerName), "asker_peer_id": string(moved.FromPeerID),
		"reason": reason, "hops": len(moved.Delegations),
	})
	return result, nil
}

// cancelAsk closes an open ask on a's authority. The asker gets the
// cancellation as the ask's outcome on its reply path; the holder is told to
// stop.
func (h *Hub) cancelAsk(ctx context.Context, a wireActor, cid, reason string) (WireActionResult, error) {
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
	if err := h.authorize(ctx, a, cid, nil); err != nil {
		return WireActionResult{}, err
	}
	holder, holderName := claimed.ToPeerID, claimed.ToPeerName
	framed := fmt.Sprintf("[ask #%s cancelled by %s] %s", cid, a.label(), reason)
	if err := h.deliverResolution(ctx, claimed, a.sender(), framed, "Cancelled: "+reason, "cancelled", reason, nil); err != nil {
		return WireActionResult{}, err
	}

	result := WireActionResult{CorrelationID: cid, Action: "cancel", Notified: map[string]string{}}
	result.DroppedQueued = h.dropQueuedCopies(ctx, cid, holder, "cancelled")
	result.Notified["@"+string(holderName)] = h.actionNotice(ctx, a, holder, fmt.Sprintf(
		"[ask #%s from @%s cancelled by %s: %s] Stop work on it; a reply will be refused.", cid, claimed.FromPeerName, a.label(), reason))
	h.ask.reg.AddEvent(ctx, "ask_cancelled", map[string]any{
		"correlation_id": cid, "actor": a.by(), "holder": string(holderName), "holder_peer_id": string(holder),
		"asker": string(claimed.FromPeerName), "asker_peer_id": string(claimed.FromPeerID), "reason": reason,
	})
	return result, nil
}

// nudgeAsk reminds an open ask's holder, at most once per nudgeEvery, and
// records the nudge on the ask's trace. It holds the ask's claim, so two
// nudges cannot both pass the limit, and a holder whose reply already waits
// for the asker is not nudged.
func (h *Hub) nudgeAsk(ctx context.Context, a wireActor, cid, note string) (WireActionResult, error) {
	note, err := actionText("note", note, false)
	if err != nil {
		return WireActionResult{}, err
	}
	if _, err := h.actionableAsk(ctx, cid); err != nil {
		return WireActionResult{}, err
	}
	ask, err := h.ask.asks.Claim(cid)
	if err != nil {
		return WireActionResult{}, resolveClaimErr(cid, err)
	}
	defer h.ask.asks.Release(cid)
	if ask.PendingReply != nil {
		return WireActionResult{}, resolveClaimErr(cid, service.ErrResultWaiting)
	}
	if err := h.authorize(ctx, a, cid, nil); err != nil {
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
	text := fmt.Sprintf("[nudge from %s: ask #%s from @%s, open %s]", a.label(), cid, ask.FromPeerName, time.Since(ask.CreatedAt).Round(time.Minute))
	if note != "" {
		text = strings.TrimSuffix(text, "]") + ": " + note + "]"
	}
	result := WireActionResult{CorrelationID: cid, Action: "nudge", Holder: string(ask.ToPeerName), Notified: map[string]string{}}
	result.Notified["@"+string(ask.ToPeerName)] = h.actionNotice(ctx, a, ask.ToPeerID, text)
	h.traceAsk(ctx, cid, "nudged", "", ask, map[string]any{"by": a.by(), "note": clipTraceText(note)})
	return result, nil
}
