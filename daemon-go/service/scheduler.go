package service

// Scheduled check-in dispatcher. Port of repowire/daemon/scheduler.py.
//
// A single goroutine waits until the next-due schedule's fire_at by the wall
// clock (wall_clock.go), then fires it. One-shot schedules are deleted after firing; recurring cron schedules
// advance to their next fire time. No polling: the wake channel is only
// signalled when the schedule set changes (create/delete via Wake) or after
// something fires. Past-due schedules fire immediately.
//
// Delivery reuses PeerDelivery so scheduled ask/notify traffic follows the same
// application delivery path as route-driven traffic. On delivery failure (peer
// missing or no live transport) one-shot schedules are still dropped and
// recurring ones advance — we log loudly so misfires are visible (fail loud over
// silent degrade).

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// storeErrorBackoff is how long the loop waits after next_due fails.
const storeErrorBackoff = time.Hour

// schedulerStore is the schedule data layer the loop drives. *state.Store
// satisfies it.
type schedulerStore interface {
	NextDueSchedule(ctx context.Context) (*state.Schedule, error)
	GetSchedule(ctx context.Context, scheduleID string) (*state.Schedule, error)
	RescheduleNext(ctx context.Context, scheduleID string, nextFireAt time.Time) (bool, error)
	DeleteSchedule(ctx context.Context, scheduleID string) (*state.Schedule, error)
}

// schedulerDelivery is the delivery seam (notify + scheduled-ask). *PeerDelivery
// satisfies it.
type schedulerDelivery interface {
	Notify(ctx context.Context, params NotifyParams) (NotifyResult, error)
	OpenScheduledAsk(ctx context.Context, cid, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error)
}

// Scheduler drives scheduled check-ins off a deadline-driven sleep + wake
// channel (never a poll timer).
type Scheduler struct {
	store    schedulerStore
	delivery schedulerDelivery
	clock    wallClock
	events   func(ctx context.Context, typ string, payload map[string]any) string
	// failing counts, per schedule, the slots that have failed in a row
	// since the first was reported: the rest of an outage stays quiet, and
	// the next delivery reports the recovery. In memory: a restart costs at
	// most one more report.
	failMu  sync.Mutex
	failing map[string]int
	// owner maps a schedule's from_peer to the peer to tell when it fails:
	// a local inbox surface ("dashboard") belongs to the human. nil: itself.
	owner func(from string) string

	wake   chan struct{}
	stop   chan struct{}
	doneCh chan struct{}
}

// NewScheduler wires the dispatcher. delivery is the PeerDelivery the routes
// already build; store is the schedules data layer.
func NewScheduler(store schedulerStore, delivery schedulerDelivery) *Scheduler {
	return &Scheduler{
		store:    store,
		delivery: delivery,
		clock:    systemWallClock(),
		// Buffered so a Wake() that races the loop's select isn't lost: the loop
		// always drains+rechecks next_due on each iteration.
		wake:    make(chan struct{}, 1),
		failing: map[string]int{},
		stop:    make(chan struct{}),
	}
}

// Wake signals the loop that the schedule set changed (create/delete). This is
// the notify_changed analogue: non-blocking, coalescing (a pending wake is not
// duplicated).
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start launches the firing goroutine. Idempotent: a second Start is a no-op
// while one is running.
func (s *Scheduler) Start(ctx context.Context) {
	if s.doneCh != nil {
		return
	}
	s.doneCh = make(chan struct{})
	go s.run(ctx)
}

// Stop signals the loop to exit and waits for the goroutine to finish.
func (s *Scheduler) Stop() {
	if s.doneCh == nil {
		return
	}
	close(s.stop)
	<-s.doneCh
	s.doneCh = nil
}

func (s *Scheduler) run(ctx context.Context) {
	defer close(s.doneCh)
	for {
		// Drain any pending wake before computing the next deadline so a change
		// signalled while we were firing is consumed here, not on the next loop.
		select {
		case <-s.wake:
		default:
		}

		nxt, err := s.store.NextDueSchedule(ctx)
		if err != nil {
			log.Printf("scheduler: next_due failed: %v; backing off", err)
			if _, stopped := s.clock.waitUntil(ctx, s.clock.now().Add(storeErrorBackoff), nil, s.stop); stopped {
				return
			}
			continue
		}
		if nxt == nil {
			// Nothing scheduled: block until woken (or stopped).
			if s.waitForWake(ctx) {
				return
			}
			continue
		}

		fireAt, perr := parseScheduleFireAt(nxt.FireAt)
		if perr != nil {
			// A schedule with an unparseable fire_at can never fire; drop it loudly
			// rather than wedging the loop on it forever.
			log.Printf("scheduler: schedule %s has unparseable fire_at %q (%v); dropping", nxt.ScheduleID, nxt.FireAt, perr)
			_, _ = s.store.DeleteSchedule(ctx, nxt.ScheduleID)
			continue
		}

		// Returns only at the wall-clock fire_at, so neither an early timer nor
		// a long horizon can fire it ahead of time (repowire-m4s).
		woken, stopped := s.clock.waitUntil(ctx, fireAt, s.wake, s.stop)
		if stopped {
			return
		}
		if woken {
			// Schedule set changed: reloop, recompute.
			continue
		}

		// Re-check the schedule still exists (a delete may have raced the sleep).
		current, err := s.store.GetSchedule(ctx, nxt.ScheduleID)
		if err != nil {
			log.Printf("scheduler: get %s failed: %v; skipping", nxt.ScheduleID, err)
			continue
		}
		if current == nil {
			continue
		}
		s.fire(ctx, current)
	}
}

// fire delivers a schedule then advances (cron) or removes (one-shot) it. Any
// delivery error is logged but never propagated — a misbehaving delivery must
// not kill the loop, and the schedule is still advanced/dropped in the finally
// equivalent below. Mirrors Scheduler._fire.
func (s *Scheduler) fire(ctx context.Context, sched *state.Schedule) {
	// A schedule addresses peers by the peer_ids resolved when it was made, so
	// a peer renamed since still gets it; a name is the fallback for an end the
	// daemon never resolved.
	from := firstNonEmpty(sched.FromPeerID, sched.FromPeer)
	to := firstNonEmpty(sched.ToPeerID, sched.ToPeer)
	var err error
	if sched.Kind == "ask" {
		_, err = s.delivery.OpenScheduledAsk(ctx, "", from, to, sched.Text, sched.Circle, "push")
	} else {
		// A notify to an offline peer is queued, not failed: only a send that
		// cannot happen at all (an unknown or unaddressable target, a circle
		// refusal) returns an error.
		_, err = s.delivery.Notify(ctx, NotifyParams{
			FromPeer: from,
			ToPeer:   to,
			Text:     sched.Text,
			Circle:   sched.Circle,
		})
	}
	if err != nil {
		log.Printf("scheduler: scheduled %s %s failed to deliver: %v", sched.Kind, sched.ScheduleID, err)
	} else {
		log.Printf("scheduler: fired schedule %s: %s -> %s (%s)", sched.ScheduleID, sched.FromPeer, sched.ToPeer, sched.Kind)
	}

	// Advance (cron) or drop (one-shot). Always runs regardless of delivery
	// outcome, mirroring the Python finally block.
	next := s.advance(ctx, sched)
	failedBefore := s.noteOutcome(sched.ScheduleID, err != nil)
	switch {
	case err != nil && failedBefore == 0:
		s.reportFailure(ctx, sched, err, next)
	case err != nil:
		// Still failing: reported at the first slot, quiet until it recovers.
	case failedBefore > 0:
		s.reportRecovery(ctx, sched, failedBefore)
	}
	if next.IsZero() {
		// No next slot (a one-shot, or a cron that could not advance): its
		// count can never be read again. A cron deleted through the routes
		// mid-outage keeps one entry until the daemon restarts; bounded.
		s.failMu.Lock()
		delete(s.failing, sched.ScheduleID)
		s.failMu.Unlock()
	}
}

// noteOutcome records whether sched's slot failed and returns how many slots
// had failed in a row before this one.
func (s *Scheduler) noteOutcome(id string, failed bool) int {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	before := s.failing[id]
	if failed {
		s.failing[id] = before + 1
	} else {
		delete(s.failing, id)
	}
	return before
}

// reportRecovery tells the owner a schedule that had been failing went out
// again, once.
func (s *Scheduler) reportRecovery(ctx context.Context, sched *state.Schedule, failedSlots int) {
	if s.events != nil {
		s.events(ctx, "schedule_recovered", map[string]any{
			"schedule_id": sched.ScheduleID, "kind": sched.Kind, "from": sched.FromPeer, "to": sched.ToPeer, "failed_slots": failedSlots,
		})
	}
	s.tellOwner(ctx, sched, fmt.Sprintf("Your scheduled %s %s to %s was delivered again after %d failed slot(s).",
		sched.Kind, sched.ScheduleID, sched.ToPeer, failedSlots))
}

// advance moves a cron schedule to its next slot, or drops a one-shot, and
// returns the next fire time (zero when none).
func (s *Scheduler) advance(ctx context.Context, sched *state.Schedule) time.Time {
	if sched.Cron != nil {
		// Advance from the slot just fired, never from an earlier now: a timer
		// can wake a moment before fire_at, and NextFireAfter(now) would then
		// return the same slot and fire it twice (repowire-5je).
		after := s.clock.now().UTC()
		if fireAt, err := parseScheduleFireAt(sched.FireAt); err == nil && fireAt.After(after) {
			after = fireAt
		}
		next, err := NextFireAfter(*sched.Cron, after)
		if err != nil {
			log.Printf("scheduler: recurring schedule %s has invalid cron after fire (%v); dropping", sched.ScheduleID, err)
			_, _ = s.store.DeleteSchedule(ctx, sched.ScheduleID)
			return time.Time{}
		}
		if _, err := s.store.RescheduleNext(ctx, sched.ScheduleID, next); err != nil {
			log.Printf("scheduler: reschedule %s failed (%v); dropping", sched.ScheduleID, err)
			_, _ = s.store.DeleteSchedule(ctx, sched.ScheduleID)
			return time.Time{}
		}
		return next
	}
	if _, err := s.store.DeleteSchedule(ctx, sched.ScheduleID); err != nil {
		log.Printf("scheduler: delete one-shot %s failed: %v", sched.ScheduleID, err)
	}
	return time.Time{}
}

// schedulerSender is who tells a schedule's owner that a send failed.
const schedulerSender = "repowire:scheduler"

// reportFailure makes a failed scheduled send visible to whoever scheduled
// it (#641), instead of a log line nobody reads: a schedule_failed event in
// the journal, and a notify to the sender, which is queued if the sender is
// offline and lands in the inbox when it is the human. Nothing is held for
// the target: a live ask to an offline peer fails too, and holding a
// recurring ask would deliver a pile of stale ones when the peer returns.
// A recurring schedule tries again at its next slot.
func (s *Scheduler) reportFailure(ctx context.Context, sched *state.Schedule, cause error, next time.Time) {
	retry := "it was a one-shot, so it has been removed"
	if !next.IsZero() {
		retry = "it tries again at its next slot, " + next.UTC().Format(time.RFC3339)
	} else if sched.Cron != nil {
		retry = "its cron could not be advanced, so it has been removed"
	}
	if s.events != nil {
		payload := map[string]any{
			"schedule_id": sched.ScheduleID, "kind": sched.Kind, "from": sched.FromPeer, "to": sched.ToPeer,
			"error": cause.Error(), "recurring": sched.Cron != nil,
		}
		if !next.IsZero() {
			payload["next_fire_at"] = next.UTC().Format(time.RFC3339)
		}
		s.events(ctx, "schedule_failed", payload)
	}
	quiet := ""
	if sched.Cron != nil && !next.IsZero() {
		quiet = " Further failures of this schedule stay quiet until it goes out again."
	}
	s.tellOwner(ctx, sched, fmt.Sprintf("Your scheduled %s %s to %s did not go out: %v. Nothing was delivered, and %s.%s",
		sched.Kind, sched.ScheduleID, sched.ToPeer, cause, retry, quiet))
}

// tellOwner notifies whoever owns sched, never the scheduler itself.
func (s *Scheduler) tellOwner(ctx context.Context, sched *state.Schedule, text string) {
	if sched.FromPeer == "" || sched.FromPeer == schedulerSender {
		return
	}
	to := firstNonEmpty(sched.FromPeerID, sched.FromPeer)
	if s.owner != nil {
		if owner := s.owner(sched.FromPeer); owner != sched.FromPeer {
			to = owner // a schedule made on one of the human's surfaces
		}
	}
	if _, err := s.delivery.Notify(ctx, NotifyParams{FromPeer: schedulerSender, ToPeer: to, Text: text, BypassCircle: true}); err != nil {
		log.Printf("scheduler: could not tell %s about schedule %s: %v", sched.FromPeer, sched.ScheduleID, err)
	}
}

// WithOwner plugs in who owns a schedule's from_peer (Inbox.OwnerOf): the
// human, for a schedule made on one of its local surfaces.
func (s *Scheduler) WithOwner(owner func(from string) string) *Scheduler {
	s.owner = owner
	return s
}

// WithEvents plugs the journal in, so a failed scheduled send is recorded
// as a schedule_failed event (the registry's AddEvent).
func (s *Scheduler) WithEvents(add func(ctx context.Context, typ string, payload map[string]any) string) *Scheduler {
	s.events = add
	return s
}

// waitForWake blocks until a wake signal, Stop, or ctx cancellation. Returns
// true if the loop should exit.
func (s *Scheduler) waitForWake(ctx context.Context) (stopped bool) {
	select {
	case <-s.wake:
		return false
	case <-s.stop:
		return true
	case <-ctx.Done():
		return true
	}
}

// parseScheduleFireAt parses the stored ISO-8601 fire_at.
func parseScheduleFireAt(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
