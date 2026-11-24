package service

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"time"

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
		if err != nil || work == nil || currentWorkAttempt(work) != attemptID {
			continue
		}
		if work.AssignedPeerID != nil && *work.AssignedPeerID != string(peerID) {
			continue
		}
		if state.InterruptedByRunner(work) {
			// The daemon stopped before recording the send, but this run
			// shows the executor has the job: it resumes the attempt.
			if work.AssignedPeerID == nil {
				continue
			}
			if updated, err := c.store.ResumeInterruptedAttempt(ctx, workID, attemptID); err != nil {
				log.Printf("job completion: resume interrupted %s: %v", workID, err)
			} else if updated != nil {
				c.emit(updated, work.State)
			}
			continue
		}
		if work.State != "delivered" {
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
		if attemptID := currentWorkAttempt(work); attemptID != "" {
			if err := c.finish(ctx, work, attemptID, text); err != nil {
				log.Printf("job completion: complete %s: %v", work.WorkID, err)
			}
		}
	}
}

// finish completes the attempt and the work with the run's final message,
// then closes, releases, and notifies.
func (c *JobCompletion) finish(ctx context.Context, work *state.TrackedWork, attemptID, text string) error {
	status, phase := "completed", "completed"
	if _, err := c.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &phase}); err != nil {
		return err
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
	if err != nil {
		return err
	}
	if updated == nil {
		return nil
	}
	return c.afterTerminal(ctx, updated, "running", "fire_completed", "completed")
}

// afterTerminal runs what follows a terminal job state: the ask closes, the
// change is emitted, a per-fire executor is released, and the owner hears.
// It is safe to repeat: the ask stays closed and a released executor stays
// released, while a repeated owner notice is sent again.
func (c *JobCompletion) afterTerminal(ctx context.Context, work *state.TrackedWork, prior, askReason, releaseReason string) error {
	if c.asks != nil && work.CorrelationID != nil {
		_, _ = c.asks.Close(ctx, *work.CorrelationID, askReason)
	}
	c.emit(work, prior)
	if err := c.release(ctx, work, releaseReason); err != nil {
		return err
	}
	c.notifyOwner(ctx, work)
	return nil
}

// release frees the work's executor. A release that reports failure (a
// hosted agent the runtime could not retire) is an error, so a caller that
// is retried, such as a hosted run's report, tries it again.
func (c *JobCompletion) release(ctx context.Context, work *state.TrackedWork, reason string) error {
	if c.control == nil {
		return nil
	}
	released, err := c.control.ReleaseExecutorForWork(ctx, work, reason)
	if err != nil {
		return fmt.Errorf("release %s: %w", work.WorkID, err)
	}
	if released["status"] == "failed" {
		return fmt.Errorf("release %s: %v", work.WorkID, released["reason"])
	}
	return nil
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
			if attemptID := currentWorkAttempt(work); attemptID != "" {
				if err := c.failWork(ctx, work, attemptID, "executor_died", detail); err != nil {
					log.Printf("job completion: fail %s after terminal offline: %v", work.WorkID, err)
				}
			}
		}
	}
}

// OnHostedRun settles the job a hosted run carried, found by the job and
// attempt markers in its prompt: an answer completes it and a run without
// one fails it as hosted_unanswered. The settlement observer reruns it when
// a report is replayed, so each step tolerates finding its work further on.
// An attempt recovered as runner_interrupted (the daemon stopped before the
// send confirmed, but the runtime had it) still takes its run's result, as
// long as no newer attempt replaced it.
func (c *JobCompletion) OnHostedRun(ctx context.Context, delivery HostedDelivery, result HostedResult) error {
	if c == nil || c.store == nil || delivery.ToPeerID == "" {
		return nil
	}
	work, attemptID, err := c.hostedWork(ctx, delivery)
	if err != nil || work == nil {
		return err
	}
	if state.InterruptedByRunner(work) {
		resumed, err := c.store.ResumeInterruptedAttempt(ctx, work.WorkID, attemptID)
		if err != nil || resumed == nil {
			return err
		}
		c.emit(resumed, work.State)
		work = resumed
	}
	switch {
	case work.State == "delivered" || work.State == "running":
		if result.Status != "done" {
			return c.failWork(ctx, work, attemptID, "hosted_unanswered", result.Reason)
		}
		if work.State == "delivered" {
			reason, phase := "turn_started", "turn_started"
			if work, err = c.store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: "running", StateReason: &reason, Phase: &phase, AttemptID: &attemptID}); err != nil || work == nil {
				return err
			}
			c.emit(work, "delivered")
		}
		return c.finish(ctx, work, attemptID, result.Text)
	case work.State == "completed":
		return c.afterTerminal(ctx, work, "running", "fire_completed", "completed")
	case work.State == "failed":
		return c.afterTerminal(ctx, work, "running", "hosted_unanswered", "hosted_unanswered")
	case work.State == "expired":
		// ExpireOverdue ended the job and told its owner; the run's report
		// retries a release that failed then, until the agent is retired.
		return c.release(ctx, work, "deadline_exceeded")
	}
	return nil
}

// hostedWork is the job the delivery carried, or nil when it carried none or
// the job has moved to another attempt or executor.
func (c *JobCompletion) hostedWork(ctx context.Context, delivery HostedDelivery) (*state.TrackedWork, string, error) {
	workID, attemptID := workMarker.FindString(delivery.Text), attemptMarker.FindString(delivery.Text)
	if workID == "" || attemptID == "" {
		return nil, "", nil
	}
	work, err := c.store.GetWork(ctx, workID)
	if err != nil || work == nil {
		return nil, "", err
	}
	if currentWorkAttempt(work) != attemptID || work.AssignedPeerID == nil || *work.AssignedPeerID != delivery.ToPeerID {
		return nil, "", nil
	}
	return work, attemptID, nil
}

// failWork fails the attempt and the work with reason, then closes,
// releases, and notifies.
func (c *JobCompletion) failWork(ctx context.Context, work *state.TrackedWork, attemptID, reason, detail string) error {
	prior := work.State
	errorData := map[string]any{
		"reason":  reason,
		"detail":  detail,
		"peer_id": strOrEmpty(work.AssignedPeerID),
	}
	status, phase := "failed", reason
	if _, err := c.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
		Status: &status,
		Phase:  &phase,
		Error:  errorData,
	}); err != nil {
		return err
	}
	stateReason := reason
	updated, err := c.store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{
		State:       "failed",
		StateReason: &stateReason,
		Phase:       &phase,
		Error:       errorData,
		AttemptID:   &attemptID,
	})
	if err != nil {
		return err
	}
	if updated == nil {
		return nil
	}
	return c.afterTerminal(ctx, updated, prior, reason, reason)
}

// ExpireOverdue ends every job whose deadline_at is at or before now and that
// is not yet terminal (#457): its current attempt and the work become
// expired with deadline_exceeded, its dispatch ask closes, its executor is
// released and its owner is told, as when an executor dies. Work still
// dispatching is left to its lease and expires on a later tick. The job
// runner calls this when its deadline wakes it; there is no timer of its own.
func (c *JobCompletion) ExpireOverdue(ctx context.Context, now time.Time) {
	if c == nil || c.store == nil {
		return
	}
	deadlines, err := c.store.WorkDeadlines(ctx)
	if err != nil {
		log.Printf("job completion: list deadlines: %v", err)
		return
	}
	for _, d := range deadlines {
		if d.At.After(now) || d.State == "dispatching" {
			continue
		}
		work, err := c.store.GetWork(ctx, d.WorkID)
		if err != nil || work == nil || work.Terminal() {
			continue
		}
		reason := "deadline_exceeded"
		errorData := map[string]any{"reason": reason, "deadline_at": d.At.Format(time.RFC3339)}
		update := state.WorkUpdate{State: "expired", StateReason: &reason, Phase: &reason, Error: errorData}
		if attemptID := currentWorkAttempt(work); attemptID != "" {
			// "expired", unlike "failed", does not move the work itself; the
			// work update below records the terminal state and its reason.
			status := "expired"
			if _, err := c.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &reason, Error: errorData}); err != nil {
				log.Printf("job completion: expire attempt %s/%s: %v", work.WorkID, attemptID, err)
				continue
			}
			update.AttemptID = &attemptID
		}
		updated, err := c.store.UpdateWorkState(ctx, work.WorkID, update)
		if err != nil || updated == nil {
			log.Printf("job completion: expire work %s: %v", work.WorkID, err)
			continue
		}
		if c.asks != nil && updated.CorrelationID != nil {
			_, _ = c.asks.Close(ctx, *updated.CorrelationID, reason)
		}
		c.emit(updated, work.State)
		if updated.AssignedPeerID != nil {
			// A failed release is retried by a hosted run's report (OnHostedRun).
			if err := c.release(ctx, updated, reason); err != nil {
				log.Printf("job completion: release expired work %s: %v", updated.WorkID, err)
			}
		}
		c.notifyOwner(ctx, updated)
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
