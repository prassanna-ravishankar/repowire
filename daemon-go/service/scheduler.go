package service

// Scheduled check-in dispatcher. Port of repowire/daemon/scheduler.py.
//
// A single goroutine sleeps until the next-due schedule's fire_at, then fires
// it. One-shot schedules are deleted after firing; recurring cron schedules
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
	"log"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// maxSchedulerSleep caps a single sleep so a long horizon doesn't outlive a
// clock jump unnoticed. Mirrors scheduler._MAX_SLEEP_SECONDS.
const maxSchedulerSleep = time.Hour

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
	OpenScheduledAsk(ctx context.Context, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error)
}

// Scheduler drives scheduled check-ins off a deadline-driven sleep + wake
// channel (never a poll timer).
type Scheduler struct {
	store    schedulerStore
	delivery schedulerDelivery

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
		// Buffered so a Wake() that races the loop's select isn't lost: the loop
		// always drains+rechecks next_due on each iteration.
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
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
			if s.sleep(ctx, maxSchedulerSleep) {
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

		delay := time.Until(fireAt)
		if delay > 0 {
			if delay > maxSchedulerSleep {
				delay = maxSchedulerSleep
			}
			woken, stopped := s.sleepUntil(ctx, delay)
			if stopped {
				return
			}
			if woken {
				// Schedule set changed (or sleep was capped): reloop, recompute.
				continue
			}
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
	if sched.Kind == "ask" {
		if _, err := s.delivery.OpenScheduledAsk(ctx, sched.FromPeer, sched.ToPeer, sched.Text, sched.Circle, "push"); err != nil {
			log.Printf("scheduler: scheduled ask %s failed to deliver: %v", sched.ScheduleID, err)
		}
	} else {
		if _, err := s.delivery.Notify(ctx, NotifyParams{
			FromPeer: sched.FromPeer,
			ToPeer:   sched.ToPeer,
			Text:     sched.Text,
			Circle:   sched.Circle,
		}); err != nil {
			log.Printf("scheduler: scheduled notify %s failed to deliver: %v", sched.ScheduleID, err)
		}
	}
	log.Printf("scheduler: fired schedule %s: %s -> %s (%s)", sched.ScheduleID, sched.FromPeer, sched.ToPeer, sched.Kind)

	// Advance (cron) or drop (one-shot). Always runs regardless of delivery
	// outcome, mirroring the Python finally block.
	if sched.Cron != nil {
		next, err := NextFireAfter(*sched.Cron, time.Now().UTC())
		if err != nil {
			log.Printf("scheduler: recurring schedule %s has invalid cron after fire (%v); dropping", sched.ScheduleID, err)
			_, _ = s.store.DeleteSchedule(ctx, sched.ScheduleID)
			return
		}
		if _, err := s.store.RescheduleNext(ctx, sched.ScheduleID, next); err != nil {
			log.Printf("scheduler: reschedule %s failed (%v); dropping", sched.ScheduleID, err)
			_, _ = s.store.DeleteSchedule(ctx, sched.ScheduleID)
		}
		return
	}
	if _, err := s.store.DeleteSchedule(ctx, sched.ScheduleID); err != nil {
		log.Printf("scheduler: delete one-shot %s failed: %v", sched.ScheduleID, err)
	}
}

// sleepUntil sleeps for d, returning (woken, stopped). woken is true if a wake
// signal arrived first; stopped is true if Stop/ctx fired.
func (s *Scheduler) sleepUntil(ctx context.Context, d time.Duration) (woken, stopped bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false, false
	case <-s.wake:
		return true, false
	case <-s.stop:
		return false, true
	case <-ctx.Done():
		return false, true
	}
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

// sleep blocks for d unless Stop/ctx fires first; returns true to exit.
func (s *Scheduler) sleep(ctx context.Context, d time.Duration) (stopped bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
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
