package state

// work_runner.go adds the runner-managed tracked-work methods the JobRunner and
// SessionControl depend on: per-attempt provenance mutation (update_attempt),
// stale-dispatch recovery (recover_stale_dispatching), calendar materialization,
// and the status()/result() projections the HTTP routes serialize. These mirror
// repowire/daemon/state/work.py + state/calendar.py. They are additive (a new
// file in the state package), reusing the helpers already defined in work.go and
// calendar.go.

import (
	"context"
	"fmt"
	"time"
)

// AttemptUpdate carries the optional per-attempt fields for UpdateAttempt
// (mirrors update_attempt()'s kwargs). nil fields leave the attribute unchanged,
// matching the Python "is not None" guards.
type AttemptUpdate struct {
	Status              *string
	Phase               *string
	AssignedPeerID      *string
	AssignedPeerInfo    map[string]any
	Tmux                map[string]any
	CorrelationID       *string
	DeliveryState       *string
	ResumePlan          map[string]any
	OperationID         *string
	AcquisitionStrategy *string
	Acquisition         map[string]any
	Error               map[string]any
}

// attemptTerminalStatuses mirrors the set in update_attempt() that stamps
// completed_at on the attempt.
var attemptTerminalStatuses = map[string]struct{}{
	"delivered": {}, "completed": {}, "failed": {},
	"unavailable": {}, "interrupted": {}, "cancelled": {},
}

// UpdateAttempt mutates a single runner attempt in provenance and, when the
// attempt is the current one, cascades the matching work-level state transition
// (delivered/failed/unavailable/cancelled). Returns (nil,nil) when the work or
// the attempt id is unknown. Mirrors SQLiteWorkStore.update_attempt.
func (s *Store) UpdateAttempt(ctx context.Context, workID, attemptID string, u AttemptUpdate) (*TrackedWork, error) {
	existing, err := s.GetWork(ctx, workID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}
	provenance := runnerProvenance(existing)
	runner := provenance["runner"].(map[string]any)
	attempts := toAnySlice(runner["attempts"])
	completedAt := existing.CompletedAt

	found := false
	for i, raw := range attempts {
		attempt, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := attempt["attempt_id"].(string); id != attemptID {
			continue
		}
		found = true
		setStr(attempt, "status", u.Status)
		setStr(attempt, "phase", u.Phase)
		setStr(attempt, "assigned_peer_id", u.AssignedPeerID)
		setMap(attempt, "assigned_peer_info", u.AssignedPeerInfo)
		setMap(attempt, "tmux", u.Tmux)
		setStr(attempt, "correlation_id", u.CorrelationID)
		setStr(attempt, "delivery_state", u.DeliveryState)
		setMap(attempt, "resume_plan", u.ResumePlan)
		setStr(attempt, "operation_id", u.OperationID)
		setStr(attempt, "acquisition_strategy", u.AcquisitionStrategy)
		setMap(attempt, "acquisition", u.Acquisition)
		setMap(attempt, "error", u.Error)
		if u.Status != nil {
			if _, terminal := attemptTerminalStatuses[*u.Status]; terminal {
				attempt["completed_at"] = nowISO()
			}
		}
		attempts[i] = attempt
		break
	}
	if !found {
		return nil, nil
	}
	runner["attempts"] = attempts
	if len(u.Error) > 0 {
		runner["last_error"] = u.Error
	}
	provenance["runner"] = runner

	curAttempt, _ := runner["current_attempt_id"].(string)
	if curAttempt != attemptID {
		// Not the current attempt: persist provenance only, no state cascade.
		_, err := s.db.ExecContext(ctx,
			`UPDATE tracked_work SET provenance_json = ?, updated_at = ? WHERE work_id = ?`,
			dumpJSONObject(provenance), nowISO(), workID)
		if err != nil {
			return nil, fmt.Errorf("update attempt %s/%s: %w", workID, attemptID, err)
		}
		return s.GetWork(ctx, workID)
	}

	st := existing.State
	reason := existing.StateReason
	phase := existing.Phase
	if u.Phase != nil {
		phase = u.Phase
	}
	if u.Status != nil {
		switch *u.Status {
		case "delivered":
			st = "delivered"
			reason = ptrStr("ask_delivered")
		case "failed":
			st = "failed"
			reason = ptrStr(errReason(u.Error, "dispatch_failed"))
			if completedAt == nil {
				completedAt = ptrStr(nowISO())
			}
		case "unavailable":
			st = "unavailable"
			reason = ptrStr(errReason(u.Error, "unavailable"))
			if completedAt == nil {
				completedAt = ptrStr(nowISO())
			}
		case "cancelled":
			st = "cancelled"
			reason = ptrStr("cancel_requested")
			if completedAt == nil {
				completedAt = ptrStr(nowISO())
			}
		}
	}

	assignedPeerID := existing.AssignedPeerID
	if u.AssignedPeerID != nil {
		assignedPeerID = u.AssignedPeerID
	}
	correlationID := existing.CorrelationID
	if u.CorrelationID != nil {
		correlationID = u.CorrelationID
	}
	errObj := existing.Error
	if u.Error != nil {
		errObj = u.Error
	}
	if err := s.replaceWorkJSON(ctx, workID, replaceWorkArgs{
		State:          st,
		StateReason:    reason,
		Phase:          phase,
		AssignedPeerID: assignedPeerID,
		CorrelationID:  correlationID,
		Provenance:     provenance,
		Error:          errObj,
		CompletedAt:    completedAt,
	}); err != nil {
		return nil, err
	}
	return s.GetWork(ctx, workID)
}

// RecoverStaleDispatching transitions every 'dispatching' work whose runner
// lease has expired (relative to now) to 'unavailable' with reason
// runner_interrupted. Only dispatching is lease-bounded — once delivered, the
// executor owns the fire. Mirrors SQLiteWorkStore.recover_stale_dispatching.
func (s *Store) RecoverStaleDispatching(ctx context.Context, now string) ([]*TrackedWork, error) {
	dispatching := "dispatching"
	rows, err := s.ListWork(ctx, WorkFilter{State: &dispatching})
	if err != nil {
		return nil, err
	}
	current, perr := workParseISO(now)
	if perr != nil {
		current = time.Now().UTC()
	}
	var recovered []*TrackedWork
	for _, work := range rows {
		provenance := runnerProvenance(work)
		runner := provenance["runner"].(map[string]any)
		leaseUntil, _ := runner["lease_until"].(string)
		if leaseUntil != "" {
			if lease, lerr := workParseISO(leaseUntil); lerr == nil && lease.After(current) {
				continue
			}
		}
		attemptID, _ := runner["current_attempt_id"].(string)
		attempts := toAnySlice(runner["attempts"])
		for i, raw := range attempts {
			attempt, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := attempt["attempt_id"].(string); id == attemptID {
				attempt["status"] = "unavailable"
				attempt["phase"] = "runner_interrupted"
				attempt["completed_at"] = now
				attempt["error"] = map[string]any{"reason": "runner_interrupted"}
				attempts[i] = attempt
				break
			}
		}
		runner["attempts"] = attempts
		runner["last_error"] = map[string]any{"reason": "runner_interrupted"}
		runner["current_attempt_id"] = attemptID
		provenance["runner"] = runner
		if err := s.replaceWorkJSON(ctx, work.WorkID, replaceWorkArgs{
			State:          "unavailable",
			StateReason:    ptrStr("runner_interrupted"),
			Phase:          ptrStr("runner_interrupted"),
			AssignedPeerID: work.AssignedPeerID,
			CorrelationID:  work.CorrelationID,
			Provenance:     provenance,
			Error:          map[string]any{"reason": "runner_interrupted"},
			CompletedAt:    ptrStr(now),
		}); err != nil {
			return nil, err
		}
		refreshed, err := s.GetWork(ctx, work.WorkID)
		if err != nil {
			return nil, err
		}
		if refreshed != nil {
			recovered = append(recovered, refreshed)
		}
	}
	return recovered, nil
}

// ResumeInterruptedAttempt hands work recovered as runner_interrupted back to
// the run of its interrupted attempt: the daemon stopped after sending the
// prompt but before recording it, so the executor has the job and its run is
// the truth. The work and that attempt become running/turn_started again.
// It returns (nil, nil), changing nothing, unless the work is still
// unavailable with runner_interrupted, attemptID is its current attempt, and
// no cancel was requested.
func (s *Store) ResumeInterruptedAttempt(ctx context.Context, workID, attemptID string) (*TrackedWork, error) {
	work, err := s.GetWork(ctx, workID)
	if err != nil || work == nil {
		return nil, err
	}
	if !InterruptedByRunner(work) || work.CancelRequested || attemptID == "" || runnerCurrentAttempt(work) != attemptID {
		return nil, nil
	}
	provenance := runnerProvenance(work)
	runner := provenance["runner"].(map[string]any)
	attempts := toAnySlice(runner["attempts"])
	for i, raw := range attempts {
		if attempt, ok := raw.(map[string]any); ok && attempt["attempt_id"] == attemptID {
			attempt["status"], attempt["phase"] = "running", "turn_started"
			delete(attempt, "completed_at")
			delete(attempt, "error")
			attempts[i] = attempt
		}
	}
	runner["attempts"] = attempts
	delete(runner, "last_error")
	provenance["runner"] = runner
	// Conditional on the recovered state, so two runs racing to resume it
	// resume it once, and a retry or cancel that got there first wins.
	const q = `UPDATE tracked_work SET
		state = 'running', state_reason = 'turn_started', phase = 'turn_started',
		provenance_json = ?, error_json = '{}', completed_at = NULL, updated_at = ?
		WHERE work_id = ? AND state = 'unavailable' AND state_reason = 'runner_interrupted' AND cancel_requested = 0`
	res, err := s.db.ExecContext(ctx, q, dumpJSONObject(provenance), nowISO(), workID)
	if err != nil {
		return nil, fmt.Errorf("resume work %s: %w", workID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return s.GetWork(ctx, workID)
}

// InterruptedByRunner reports work recovered as runner_interrupted: the
// runner stopped mid-dispatch, so whether the executor got the prompt is
// unknown until its run says so.
func InterruptedByRunner(w *TrackedWork) bool {
	return w != nil && w.State == "unavailable" && w.StateReason != nil && *w.StateReason == "runner_interrupted"
}

// replaceWorkArgs bundles the columns replaceWorkJSON writes (mirrors
// _replace_work_json's keyword args).
type replaceWorkArgs struct {
	State          string
	StateReason    *string
	Phase          *string
	AssignedPeerID *string
	CorrelationID  *string
	Provenance     map[string]any
	Error          map[string]any
	CompletedAt    *string
}

// replaceWorkJSON writes the runner-managed columns + a fresh updated_at, mirroring
// SQLiteWorkStore._replace_work_json.
func (s *Store) replaceWorkJSON(ctx context.Context, workID string, a replaceWorkArgs) error {
	const q = `UPDATE tracked_work SET
		state = ?, state_reason = ?, phase = ?, assigned_peer_id = ?,
		correlation_id = ?, provenance_json = ?, error_json = ?,
		completed_at = ?, updated_at = ?
		WHERE work_id = ?`
	_, err := s.db.ExecContext(ctx, q,
		a.State, strOrNil(a.StateReason), strOrNil(a.Phase), strOrNil(a.AssignedPeerID),
		strOrNil(a.CorrelationID), dumpJSONObject(a.Provenance), dumpJSONObject(orEmptyMap(a.Error)),
		strOrNil(a.CompletedAt), nowISO(), workID,
	)
	if err != nil {
		return fmt.Errorf("replace work %s: %w", workID, err)
	}
	return nil
}

// --- small mutation helpers (the Python "if x is not None" guards) ---

func setStr(m map[string]any, key string, v *string) {
	if v != nil {
		m[key] = *v
	}
}

func setMap(m map[string]any, key string, v map[string]any) {
	if v != nil {
		m[key] = v
	}
}

func ptrStr(s string) *string { return &s }

func errReason(e map[string]any, fallback string) string {
	if e != nil {
		if r, ok := e["reason"].(string); ok && r != "" {
			return r
		}
	}
	return fallback
}
