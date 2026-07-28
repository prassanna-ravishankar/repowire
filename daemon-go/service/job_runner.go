package service

// job_runner.go — durable autonomous job dispatch. Port of
// repowire/daemon/job_runner.py:JobRunner. NOT a polling timer: a single
// self-rescheduling goroutine wakes on (a) the daemon stopping, (b) an explicit
// Wake() after a create/update, or (c) a timer set to the seconds-until-next
// concrete deadline. Each tick runs recover_stale → materialize_due_calendar →
// run_due_once, then sleeps. _dispatch acquires an executor (SessionControl),
// builds the durable-job prompt, records the attempt, and delivers through
// PeerDelivery.OpenScheduledAsk(reply_delivery="pull") — the @jobs sender has no
// transport, so the executor's ack is retained on the ask rather than notified
// back to a peer that can't receive it.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

const (
	jobRunnerOwnerID = "daemon-job-runner"
	jobLeaseSeconds  = 300
)

// scheduledAskOpener is the PeerDelivery method JobRunner uses. *PeerDelivery
// satisfies it. Kept as a seam so the runner test can supply a fake that records
// the (from,to,text) without a live transport.
type scheduledAskOpener interface {
	OpenScheduledAsk(ctx context.Context, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error)
}

// JobRunner acquires queued jobs and dispatches each exactly once per attempt.
type JobRunner struct {
	store        *state.Store
	delivery     scheduledAskOpener
	control      *SessionControl
	senderPeerID proto.PeerID

	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	started bool
}

// NewJobRunner wires the runner. control may be nil only in degenerate test setups
// (dispatch then fails loud as missing_session_control on every attempt).
func NewJobRunner(store *state.Store, delivery scheduledAskOpener, control *SessionControl) *JobRunner {
	return &JobRunner{
		store:    store,
		delivery: delivery,
		control:  control,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetSenderPeerID addresses dispatch asks from the registered @jobs service peer.
func (r *JobRunner) SetSenderPeerID(id proto.PeerID) { r.senderPeerID = id }

// Control exposes the runner's SessionControl so routes can release executors
// without a second control instance.
func (r *JobRunner) Control() *SessionControl { return r.control }

// Start recovers stale dispatching work, then launches the dispatch goroutine.
func (r *JobRunner) Start(ctx context.Context) {
	if r.started {
		return
	}
	r.started = true
	if _, err := r.store.RecoverStaleDispatching(ctx, workNowISORunner()); err != nil {
		log.Printf("job_runner: recover_stale on start failed: %v", err)
	}
	go r.loop(ctx)
}

// Stop signals the goroutine and blocks until it exits.
func (r *JobRunner) Stop() {
	if !r.started {
		return
	}
	close(r.stop)
	<-r.done
	r.started = false
}

// Wake nudges the loop after a create/update so a newly-due or sooner-deadline
// job is picked up without waiting for the previously-computed timer.
func (r *JobRunner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default: // already pending — coalesce
	}
}

func (r *JobRunner) loop(ctx context.Context) {
	defer close(r.done)
	for {
		// Drain a pending wake before inspecting state so a create/update that
		// races deadline computation still re-arms the next iteration.
		select {
		case <-r.wake:
		default:
		}

		r.tick(ctx)

		delay := r.secondsUntilNextDeadline(ctx)
		var timer *time.Timer
		var timerC <-chan time.Time
		if delay != nil {
			timer = time.NewTimer(time.Duration(*delay * float64(time.Second)))
			timerC = timer.C
		}
		select {
		case <-r.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-r.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

func (r *JobRunner) tick(ctx context.Context) {
	if _, err := r.store.RecoverStaleDispatching(ctx, workNowISORunner()); err != nil {
		log.Printf("job_runner: recover_stale tick failed: %v", err)
	}
	if materialized, err := r.store.MaterializeDue(ctx, time.Now().UTC(), NextFireAfter); err != nil {
		log.Printf("job_runner: materialize_due failed: %v", err)
	} else if len(materialized) > 0 {
		r.Wake()
	}
	if err := r.runDueOnce(ctx); err != nil {
		log.Printf("job_runner: run_due_once failed: %v", err)
	}
}

// runDueOnce dispatches every queued job whose due_at is at or before now.
func (r *JobRunner) runDueOnce(ctx context.Context) error {
	queued := "queued"
	rows, err := r.store.ListWork(ctx, state.WorkFilter{State: &queued})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, work := range rows {
		if due := scheduleDueAt(work); due != "" {
			if d, perr := parseRunnerISO(due); perr == nil && d.After(now) {
				continue
			}
		}
		if _, err := r.RunJob(ctx, work.WorkID, false, false); err != nil {
			log.Printf("job_runner: dispatch %s failed: %v", work.WorkID, err)
		}
	}
	return nil
}

// secondsUntilNextDeadline returns seconds until the soonest concrete due/lease/
// calendar deadline, or nil for "sleep until woken". Mirrors
// _seconds_until_next_deadline.
func (r *JobRunner) secondsUntilNextDeadline(ctx context.Context) *float64 {
	now := time.Now().UTC()
	var soonest *time.Time
	consider := func(t time.Time) {
		if soonest == nil || t.Before(*soonest) {
			tc := t
			soonest = &tc
		}
	}
	queued := "queued"
	if rows, err := r.store.ListWork(ctx, state.WorkFilter{State: &queued}); err == nil {
		for _, work := range rows {
			due := now
			if d := scheduleDueAt(work); d != "" {
				if parsed, perr := parseRunnerISO(d); perr == nil {
					due = parsed
				}
			}
			consider(due)
		}
	}
	dispatching := "dispatching"
	if rows, err := r.store.ListWork(ctx, state.WorkFilter{State: &dispatching}); err == nil {
		for _, work := range rows {
			runner := mapAtAny(work.Provenance, "runner")
			if lu, ok := runner["lease_until"].(string); ok && lu != "" {
				if parsed, perr := parseRunnerISO(lu); perr == nil {
					consider(parsed)
				}
			}
		}
	}
	if secs, err := r.store.SecondsUntilNextDue(ctx, now); err == nil && secs != nil {
		consider(now.Add(time.Duration(*secs * float64(time.Second))))
	}
	if soonest == nil {
		return nil
	}
	d := soonest.Sub(now).Seconds()
	if d < 0 {
		d = 0
	}
	return &d
}

// RunJob acquires the work for dispatch and runs the attempt. ignoreDueAt skips
// the due-gate (manual run); retry admits failed/unavailable/delivered. Returns
// the refreshed work, or (nil,nil) when the work can't be acquired (wrong state,
// not due, cancel-requested). Mirrors run_job.
func (r *JobRunner) RunJob(ctx context.Context, workID string, ignoreDueAt, retry bool) (*state.TrackedWork, error) {
	leaseUntil := time.Now().UTC().Add(jobLeaseSeconds * time.Second).Format("2006-01-02T15:04:05.000000-07:00")
	acquired, err := r.store.AcquireForDispatch(ctx, workID, state.AcquireOptions{
		RunnerOwnerID: jobRunnerOwnerID,
		LeaseUntil:    leaseUntil,
		IgnoreDueAt:   ignoreDueAt,
		Retry:         retry,
	})
	if err != nil {
		return nil, err
	}
	if acquired == nil {
		return nil, nil
	}
	runner := mapAtAny(acquired.Provenance, "runner")
	attemptID, _ := runner["current_attempt_id"].(string)
	if attemptID == "" {
		return acquired, nil
	}
	work, derr := r.dispatch(ctx, acquired, attemptID)
	if derr != nil {
		// Keep dispatch audit-visible: record the attempt failure.
		return r.store.UpdateAttempt(ctx, workID, attemptID, state.AttemptUpdate{
			Status: strPtr("failed"),
			Phase:  strPtr("dispatch"),
			Error:  map[string]any{"reason": "dispatch_failed", "message": derr.Error()},
		})
	}
	return work, nil
}

func (r *JobRunner) dispatch(ctx context.Context, work *state.TrackedWork, attemptID string) (*state.TrackedWork, error) {
	current, err := r.store.GetWork(ctx, work.WorkID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, nil
	}
	if current.CancelRequested {
		return r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
			Status: strPtr("cancelled"), Phase: strPtr("cancelled"),
		})
	}

	peer := r.resolveOrSpawnPeer(ctx, current, attemptID)
	if peer == nil {
		return r.store.GetWork(ctx, work.WorkID)
	}

	current, err = r.store.GetWork(ctx, work.WorkID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, nil
	}
	if current.CancelRequested {
		return r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
			Status: strPtr("cancelled"), Phase: strPtr("cancelled"),
		})
	}

	text := buildJobPrompt(current, attemptID)
	if _, err := r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
		Phase:          strPtr("delivery"),
		AssignedPeerID: strPtr(string(peer.PeerID)),
		Tmux:           map[string]any{"tmux_session": strPtrOrNil(peer.TmuxSession), "pane_id": strPtrOrNil(peer.PaneID)},
		DeliveryState:  strPtr("pending"),
	}); err != nil {
		return nil, err
	}

	from := string(r.senderPeerID)
	if from == "" {
		from = jobRunnerOwnerID
	}
	circle := peer.Circle
	cid, err := r.delivery.OpenScheduledAsk(ctx, from, string(peer.PeerID), text, &circle, "pull")
	if err != nil {
		return r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
			Status:        strPtr("failed"),
			Phase:         strPtr("delivery"),
			DeliveryState: strPtr("failed"),
			Error:         map[string]any{"reason": "ask_delivery_failed", "message": err.Error()},
		})
	}
	return r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
		Status:        strPtr("delivered"),
		Phase:         strPtr("delivered"),
		DeliveryState: strPtr("delivered"),
		CorrelationID: &cid,
	})
}

// resolveOrSpawnPeer acquires an executor via SessionControl, recording the
// per-strategy phase + binding on the attempt. Returns nil after recording an
// unavailable/failed attempt when no executor can be acquired.
func (r *JobRunner) resolveOrSpawnPeer(ctx context.Context, work *state.TrackedWork, attemptID string) *proto.Peer {
	execution := mapAtAny(work.Request, "execution")
	target := mapAtAny(execution, "target")
	if r.control == nil {
		_, _ = r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
			Status: strPtr("unavailable"), Phase: strPtr("resolve_peer"),
			Error: map[string]any{"reason": "missing_session_control"},
		})
		return nil
	}
	acq, err := r.control.AcquireExecutorForWork(ctx, work, target, jobRunnerOwnerID)
	if err != nil {
		var unavail *ExecutorAcquisitionUnavailableError
		if asUnavailable(err, &unavail) {
			status := unavail.Status
			if status == "" {
				status = "unavailable"
			}
			phase := unavail.Phase
			if phase == "" {
				phase = "resolve_peer"
			}
			u := state.AttemptUpdate{Status: &status, Phase: &phase, Error: unavail.Err}
			if unavail.OperationID != "" {
				u.OperationID = &unavail.OperationID
			}
			if unavail.AssignedPeerID != "" {
				u.AssignedPeerID = &unavail.AssignedPeerID
			}
			_, _ = r.store.UpdateAttempt(ctx, work.WorkID, attemptID, u)
			return nil
		}
		_, _ = r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
			Status: strPtr("unavailable"), Phase: strPtr("resolve_peer"),
			Error: map[string]any{"reason": "acquire_failed", "message": err.Error()},
		})
		return nil
	}

	phaseByStrategy := map[string]string{
		"assigned_peer":  "resolved_peer",
		"reused_peer":    "reused_peer",
		"backend_resume": "resumed_peer_registered",
		"spawned_peer":   "spawned_peer_registered",
	}
	phase := phaseByStrategy[acq.Strategy]
	if phase == "" {
		phase = "resolved_peer"
	}
	peer := acq.Peer
	opID := acq.OperationID
	strat := acq.Strategy
	_, _ = r.store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{
		Phase:               &phase,
		AssignedPeerID:      strPtr(string(peer.PeerID)),
		Tmux:                map[string]any{"tmux_session": strPtrOrNil(peer.TmuxSession), "pane_id": strPtrOrNil(peer.PaneID)},
		ResumePlan:          acq.ResumePlan,
		OperationID:         &opID,
		AcquisitionStrategy: &strat,
		Acquisition: map[string]any{
			"operation_id":    acq.OperationID,
			"strategy":        acq.Strategy,
			"runtime_binding": acq.RuntimeBinding,
			"release_handle":  acq.ReleaseHandle,
		},
	})
	return peer
}

// buildJobPrompt renders the durable-job contract prompt. Mirrors _build_prompt.
func buildJobPrompt(work *state.TrackedWork, attemptID string) string {
	execution := mapAtAny(work.Request, "execution")
	prompt := mapAtAny(execution, "prompt")
	body, _ := prompt["body"].(string)
	if body == "" {
		body = work.Title
	}
	return "[Repowire durable job]\n" +
		"job_id: " + work.WorkID + "\n" +
		"attempt_id: " + attemptID + "\n\n" +
		body + "\n\n" +
		"Job contract: you are running as a durable job — there is no human " +
		"at this terminal. Your turn ending ends this fire: your final " +
		"message is recorded as the job result, so end with a clear report. " +
		"Never stop to ask permission; decide, or ask a peer and block on " +
		"the reply with wait_on_ack (never end your turn to wait). Optional: " +
		"job_update for progress or structured result_data; if you must " +
		"wait on something outside the mesh, job_update state=running with " +
		"an explicit phase to hold the fire open across turns."
}

// --- runner-local helpers ---

func scheduleDueAt(work *state.TrackedWork) string {
	execution := mapAtAny(work.Request, "execution")
	schedule := mapAtAny(execution, "schedule")
	v, _ := schedule["due_at"].(string)
	return v
}

func workNowISORunner() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// nowUTCRunner is the cron-anchor clock the work routes use for NextFireAfter.
func nowUTCRunner() time.Time { return time.Now().UTC() }

func parseRunnerISO(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errParseISO
	}
	return parsed.UTC(), nil
}

var errParseISO = errors.New("unparseable ISO timestamp")

// asUnavailable unwraps a *ExecutorAcquisitionUnavailableError from err.
func asUnavailable(err error, target **ExecutorAcquisitionUnavailableError) bool {
	if e, ok := err.(*ExecutorAcquisitionUnavailableError); ok {
		*target = e
		return true
	}
	return false
}
