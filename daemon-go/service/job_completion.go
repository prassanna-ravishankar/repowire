package service

import (
	"context"
	"log"
	"regexp"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

var workMarker = regexp.MustCompile(`\bwork-[0-9a-f]{12}`)
var attemptMarker = regexp.MustCompile(`\battempt-[0-9a-f]{12}`)

type JobCompletion struct {
	store    *state.Store
	asks     *AskTracker
	control  *SessionControl
	reg      accessRegistry
	delivery *PeerDelivery
}

func NewJobCompletion(store *state.Store, asks *AskTracker, control *SessionControl, reg accessRegistry, delivery *PeerDelivery) *JobCompletion {
	return &JobCompletion{store: store, asks: asks, control: control, reg: reg, delivery: delivery}
}

func (c *JobCompletion) OnChatTurn(ctx context.Context, peerID proto.PeerID, role, text string) {
	if c == nil || c.store == nil || peerID == "" || text == "" {
		return
	}
	if role == "user" {
		c.arm(ctx, peerID, text)
	}
	if role == "assistant" {
		c.complete(ctx, peerID, text)
	}
}

func (c *JobCompletion) arm(ctx context.Context, peerID proto.PeerID, text string) {
	workIDs, attemptIDs := workMarker.FindAllString(text, -1), attemptMarker.FindAllString(text, -1)
	for index, workID := range workIDs {
		if index >= len(attemptIDs) {
			break
		}
		attemptID := attemptIDs[index]
		work, err := c.store.GetWork(ctx, workID)
		if err != nil || work == nil || work.State != "delivered" || currentWorkAttempt(work) != attemptID {
			continue
		}
		if work.AssignedPeerID != nil && *work.AssignedPeerID != string(peerID) {
			continue
		}
		reason, phase := "turn_started", "turn_started"
		if updated, err := c.store.UpdateWorkState(ctx, workID, state.WorkUpdate{State: "running", StateReason: &reason, Phase: &phase, AttemptID: &attemptID}); err == nil && updated != nil {
			c.emit(updated, "delivered")
		}
	}
}

func (c *JobCompletion) complete(ctx context.Context, peerID proto.PeerID, text string) {
	running := "running"
	works, err := c.store.ListWork(ctx, state.WorkFilter{State: &running})
	if err != nil {
		log.Printf("job completion: list running: %v", err)
		return
	}
	for _, work := range works {
		if work.AssignedPeerID == nil || *work.AssignedPeerID != string(peerID) || work.Phase == nil || *work.Phase != "turn_started" {
			continue
		}
		attemptID := currentWorkAttempt(work)
		if attemptID == "" {
			continue
		}
		status, phase := "completed", "completed"
		if _, err := c.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &phase}); err != nil {
			continue
		}
		reason := "turn_complete"
		summary := text
		if len(summary) > 2000 {
			summary = summary[:2000]
		}
		message := text
		if len(message) > 65536 {
			message = message[:65536]
		}
		updated, err := c.store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: "completed", StateReason: &reason, Phase: &phase, ResultSummary: &summary, ResultData: map[string]any{"final_message": message}, AttemptID: &attemptID})
		if err != nil || updated == nil {
			continue
		}
		if c.asks != nil && updated.CorrelationID != nil {
			_, _ = c.asks.Close(ctx, *updated.CorrelationID, "fire_completed")
		}
		c.emit(updated, "running")
		if c.control != nil {
			if _, err := c.control.ReleaseExecutorForWork(ctx, updated, "completed"); err != nil {
				log.Printf("job completion: release %s: %v", updated.WorkID, err)
			}
		}
		c.notifyOwner(ctx, updated)
	}
}

// OnPeerTerminalOffline fails active work whose assigned executor is
// conclusively gone. A transport-only disconnect is intentionally excluded:
// the same peer identity may reconnect and finish the turn.
func (c *JobCompletion) OnPeerTerminalOffline(peerID proto.PeerID, detail string) {
	if c == nil || c.store == nil || peerID == "" {
		return
	}
	ctx := context.Background()
	for _, activeState := range []string{"delivered", "running"} {
		works, err := c.store.ListWork(ctx, state.WorkFilter{State: &activeState})
		if err != nil {
			log.Printf("job completion: list %s after terminal offline: %v", activeState, err)
			continue
		}
		for _, work := range works {
			if work.AssignedPeerID == nil || *work.AssignedPeerID != string(peerID) {
				continue
			}
			attemptID := currentWorkAttempt(work)
			if attemptID == "" {
				continue
			}
			errorData := map[string]any{
				"reason":  "executor_died",
				"detail":  detail,
				"peer_id": peerID,
			}
			status, phase := "failed", "executor_died"
			if _, err := c.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
				Status: &status,
				Phase:  &phase,
				Error:  errorData,
			}); err != nil {
				log.Printf("job completion: fail attempt %s/%s: %v", work.WorkID, attemptID, err)
				continue
			}
			reason := "executor_died"
			updated, err := c.store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{
				State:       "failed",
				StateReason: &reason,
				Phase:       &phase,
				Error:       errorData,
				AttemptID:   &attemptID,
			})
			if err != nil || updated == nil {
				log.Printf("job completion: fail work %s after terminal offline: %v", work.WorkID, err)
				continue
			}
			if c.asks != nil && updated.CorrelationID != nil {
				_, _ = c.asks.Close(ctx, *updated.CorrelationID, "executor_died")
			}
			c.emit(updated, activeState)
			if c.control != nil {
				if _, err := c.control.ReleaseExecutorForWork(ctx, updated, "executor_died"); err != nil {
					log.Printf("job completion: release failed work %s: %v", updated.WorkID, err)
				}
			}
			c.notifyOwner(ctx, updated)
		}
	}
}

// ReconcileInflight performs a one-shot startup repair after live hooks have
// had a grace period to reconnect. It is request-independent and runs only
// once per daemon start, not as a polling loop.
func (c *JobCompletion) ReconcileInflight(ctx context.Context) {
	if c == nil || c.store == nil || c.reg == nil {
		return
	}
	dead := map[proto.PeerID]bool{}
	for _, activeState := range []string{"delivered", "running"} {
		works, err := c.store.ListWork(ctx, state.WorkFilter{State: &activeState})
		if err != nil {
			log.Printf("job completion: reconcile list %s: %v", activeState, err)
			continue
		}
		for _, work := range works {
			if work.AssignedPeerID == nil || *work.AssignedPeerID == "" {
				continue
			}
			peerID := proto.PeerID(*work.AssignedPeerID)
			if dead[peerID] {
				continue
			}
			peer, ok := c.reg.GetPeer(peerID)
			if !ok || peer.Status == proto.StatusOffline {
				dead[peerID] = true
			}
		}
	}
	for peerID := range dead {
		c.OnPeerTerminalOffline(peerID, "daemon_restart_reconcile")
	}
}

func currentWorkAttempt(work *state.TrackedWork) string {
	runner := mapAtAny(work.Provenance, "runner")
	value, _ := runner["current_attempt_id"].(string)
	return value
}
func (c *JobCompletion) emit(work *state.TrackedWork, prior string) {
	if c.reg == nil {
		return
	}
	summary := ""
	if work.ResultSummary != nil {
		summary = *work.ResultSummary
	}
	if len(summary) > 300 {
		summary = summary[:300]
	}
	c.reg.AddEvent(context.Background(), "job_state_changed", map[string]any{"work_id": work.WorkID, "job_id": work.WorkID, "title": work.Title, "state": work.State, "prior_state": prior, "state_reason": work.StateReason, "phase": work.Phase, "attempt_id": currentWorkAttempt(work), "assigned_peer_id": work.AssignedPeerID, "owner_peer_id": work.OwnerPeerID, "created_by_peer_id": work.CreatedByPeerID, "circle": work.Circle, "correlation_id": work.CorrelationID, "result_summary": summary, "error": work.Error})
}
func (c *JobCompletion) notifyOwner(ctx context.Context, work *state.TrackedWork) {
	if c.delivery == nil {
		return
	}
	targets := []string{}
	for _, value := range []*string{work.OwnerPeerID, work.CreatedByPeerID} {
		if value != nil && *value != "" {
			targets = append(targets, *value)
		}
	}
	if c.reg != nil {
		for _, peer := range c.reg.GetAllPeers() {
			if peer.Role == proto.RoleOrchestrator && peer.Status != proto.StatusOffline && (work.Circle == nil || peer.Circle == *work.Circle) {
				targets = append(targets, string(peer.PeerID))
				break
			}
		}
	}
	summary := ""
	if work.ResultSummary != nil {
		summary = *work.ResultSummary
	}
	if len(summary) > 300 {
		summary = summary[:300]
	}
	text := "[job " + work.WorkID + "] " + work.Title + " → " + work.State
	if summary != "" {
		text += ": " + summary
	}
	for _, target := range targets {
		result, err := c.delivery.Notify(ctx, NotifyParams{FromPeer: "jobs", ToPeer: target, Text: text, BypassCircle: true})
		if err == nil && (result.Delivered() || result.Queued()) {
			return
		}
	}
}
