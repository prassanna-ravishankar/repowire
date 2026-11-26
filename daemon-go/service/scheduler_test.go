package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// fakeScheduleStore is an in-memory schedulerStore for the firing-loop tests
// (no SQLite). It is concurrency-safe so the loop goroutine and the test
// goroutine can both touch it. hub/routes_schedules_test.go keeps its own
// (route-only) copy — the two packages no longer share one straddling fake
// now that hub and service are split.
type fakeScheduleStore struct {
	mu      sync.Mutex
	byID    map[string]*state.Schedule
	nextSeq int
}

func newFakeScheduleStore() *fakeScheduleStore {
	return &fakeScheduleStore{byID: map[string]*state.Schedule{}}
}

func (f *fakeScheduleStore) CreateSchedule(_ context.Context, fromPeer, toPeer, text string, fireAt time.Time, kind string, circle, cron *string) (*state.Schedule, error) {
	if kind == "" {
		kind = "notify"
	}
	if kind != "ask" && kind != "notify" {
		return nil, errors.New("kind must be one of [ask notify]; got " + kind)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	s := &state.Schedule{
		ScheduleID: "sched-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+f.nextSeq)),
		FromPeer:   fromPeer,
		ToPeer:     toPeer,
		Text:       text,
		FireAt:     fireAt.UTC().Format(time.RFC3339Nano),
		Kind:       kind,
		Circle:     circle,
		Cron:       cron,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	f.byID[s.ScheduleID] = s
	return s, nil
}

func (f *fakeScheduleStore) DeleteSchedule(_ context.Context, id string) (*state.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	delete(f.byID, id)
	return s, nil
}

func (f *fakeScheduleStore) GetSchedule(_ context.Context, id string) (*state.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byID[id], nil
}

func (f *fakeScheduleStore) NextDueSchedule(_ context.Context) (*state.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *state.Schedule
	for _, s := range f.byID {
		if best == nil || s.FireAt < best.FireAt {
			best = s
		}
	}
	return best, nil
}

func (f *fakeScheduleStore) RescheduleNext(_ context.Context, id string, next time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[id]
	if !ok || s.Cron == nil {
		return false, nil
	}
	s.FireAt = next.UTC().Format(time.RFC3339Nano)
	return true, nil
}

func (f *fakeScheduleStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

// fakeSchedDelivery records the dispatch calls the firing loop makes.
type fakeSchedDelivery struct {
	mu       sync.Mutex
	notifies []string // texts
	asks     []string // texts
}

func (d *fakeSchedDelivery) Notify(_ context.Context, p NotifyParams) (NotifyResult, error) {
	d.mu.Lock()
	d.notifies = append(d.notifies, p.Text)
	d.mu.Unlock()
	return NotifyResult{Status: "sent", DeliveryState: "delivered"}, nil
}

func (d *fakeSchedDelivery) OpenScheduledAsk(_ context.Context, _, _, text string, _ *string, _ string) (string, error) {
	d.mu.Lock()
	d.asks = append(d.asks, text)
	d.mu.Unlock()
	return "ask-1", nil
}

func (d *fakeSchedDelivery) notifyCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.notifies)
}

func (d *fakeSchedDelivery) askCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.asks)
}

// waitFor polls cond until true or the deadline, failing the test otherwise.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// TestSchedulerFiresPastDueOneShot is the primary firing path: a past-due
// one-shot notify fires immediately, is delivered via PeerDelivery, then dropped.
func TestSchedulerFiresPastDueOneShot(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	// Past-due fire_at so the loop fires it on the first iteration.
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "ping", time.Now().Add(-time.Minute), "notify", nil, nil)

	s := NewScheduler(store, del)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	waitFor(t, "one-shot notify delivered", func() bool { return del.notifyCount() == 1 })
	waitFor(t, "one-shot dropped after fire", func() bool { return store.count() == 0 })
}

// TestSchedulerRecurringCronAdvances: a past-due cron ask fires then is
// rescheduled (not deleted) to a future fire_at.
func TestSchedulerRecurringCronAdvances(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	cron := "0 * * * *"
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "hourly", time.Now().Add(-time.Minute), "ask", nil, &cron)

	s := NewScheduler(store, del)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	waitFor(t, "cron ask delivered", func() bool { return del.askCount() == 1 })
	// Recurring schedules are advanced, not dropped.
	waitFor(t, "cron schedule advanced (still present, fire_at in the future)", func() bool {
		got, _ := store.NextDueSchedule(context.Background())
		if got == nil {
			return false
		}
		fa, err := time.Parse(time.RFC3339Nano, got.FireAt)
		return err == nil && fa.After(time.Now())
	})
	if store.count() != 1 {
		t.Fatalf("recurring schedule should persist, got count=%d", store.count())
	}
}

// TestSchedulerWakeOnCreate: an empty scheduler is idle; creating + waking
// causes the new schedule to fire.
func TestSchedulerWakeOnCreate(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	s := NewScheduler(store, del)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	// Loop should be blocked (no schedules). Add a past-due one then Wake.
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "later", time.Now().Add(-time.Second), "notify", nil, nil)
	s.Wake()

	waitFor(t, "woken schedule fires", func() bool { return del.notifyCount() == 1 })
}

// TestSchedulerCronEarlyWakeAdvancesPastFiredSlot: a timer that wakes a moment
// before fire_at must not leave the cron on the slot it just fired. The live
// daemon fired a */30 cron at 16:59:59 and again at 17:00:00 (repowire-5je).
func TestSchedulerCronEarlyWakeAdvancesPastFiredSlot(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	cron := "*/30 * * * *"
	slot := time.Date(2026, 10, 11, 17, 0, 0, 0, time.UTC)
	sched, _ := store.CreateSchedule(context.Background(), "alpha", "beta", "nudge", slot, "notify", nil, &cron)

	s := NewScheduler(store, del)
	s.clock.now = func() time.Time { return slot.Add(-800 * time.Millisecond) }
	s.fire(context.Background(), sched)

	got, _ := store.GetSchedule(context.Background(), sched.ScheduleID)
	if got == nil {
		t.Fatal("recurring schedule was dropped")
	}
	if want := slot.Add(30 * time.Minute).Format(time.RFC3339Nano); got.FireAt != want {
		t.Fatalf("fire_at after early fire = %s, want %s", got.FireAt, want)
	}
	if del.notifyCount() != 1 {
		t.Fatalf("notifies = %d, want 1", del.notifyCount())
	}
}

// failingSchedDelivery fails scheduled sends to "down" and records every
// notify (sender, target, text), including the scheduler's own reports.
type failingSchedDelivery struct {
	mu       sync.Mutex
	notifies []NotifyParams
}

var errPeerDown = errors.New("hub: peer not connected: down")

func (d *failingSchedDelivery) Notify(_ context.Context, p NotifyParams) (NotifyResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notifies = append(d.notifies, p)
	if p.ToPeer == "down" {
		return NotifyResult{}, errors.New("Unknown peer: down")
	}
	return NotifyResult{Status: "sent"}, nil
}

func (d *failingSchedDelivery) OpenScheduledAsk(_ context.Context, _, to, _ string, _ *string, _ string) (string, error) {
	if to == "down" {
		return "", errPeerDown
	}
	return "ask-1", nil
}

// reportsTo lists the scheduler's reports to sender.
func (d *failingSchedDelivery) reportsTo(sender string) []NotifyParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []NotifyParams
	for _, n := range d.notifies {
		if n.FromPeer == schedulerSender && n.ToPeer == sender {
			out = append(out, n)
		}
	}
	return out
}

type eventLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (l *eventLog) add(_ context.Context, typ string, payload map[string]any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	payload["type"] = typ
	l.events = append(l.events, payload)
	return "evt"
}

func (l *eventLog) all() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.events...)
}

// A scheduled send that cannot go out is reported to its sender and
// journaled (#641), not only logged: a one-shot ask says it was removed, a
// recurring one says when it tries again. Nothing is held for the target.
func TestFailedScheduledSendsAreReportedToTheSender(t *testing.T) {
	store := newFakeScheduleStore()
	del := &failingSchedDelivery{}
	events := &eventLog{}
	cron := "0 9 * * *"
	_, _ = store.CreateSchedule(context.Background(), "orchestrator", "down", "heartbeat?", time.Now().Add(-time.Minute), "ask", nil, &cron)
	_, _ = store.CreateSchedule(context.Background(), "prass", "down", "one more thing?", time.Now().Add(-time.Minute), "ask", nil, nil)
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "fine", time.Now().Add(-time.Minute), "ask", nil, nil)

	s := NewScheduler(store, del).WithEvents(events.add)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()
	waitFor(t, "both failures reported", func() bool { return len(events.all()) == 2 })

	cronReport := del.reportsTo("orchestrator")
	if len(cronReport) != 1 || !strings.Contains(cronReport[0].Text, "did not go out") ||
		!strings.Contains(cronReport[0].Text, "tries again at its next slot") || !strings.Contains(cronReport[0].Text, "peer not connected") {
		t.Fatalf("recurring report = %+v", cronReport)
	}
	if !cronReport[0].BypassCircle {
		t.Fatal("the scheduler's report must reach its sender whatever its circle")
	}
	oneShot := del.reportsTo("prass")
	if len(oneShot) != 1 || !strings.Contains(oneShot[0].Text, "one-shot, so it has been removed") {
		t.Fatalf("one-shot report = %+v", oneShot)
	}
	if got := del.reportsTo("alpha"); len(got) != 0 {
		t.Fatalf("a delivered ask was reported as failed: %+v", got)
	}
	for _, ev := range events.all() {
		if ev["type"] != "schedule_failed" || ev["to"] != "down" || ev["kind"] != "ask" {
			t.Fatalf("event = %v", ev)
		}
		if ev["from"] == "orchestrator" && (ev["recurring"] != true || ev["next_fire_at"] == nil) {
			t.Fatalf("recurring event = %v", ev)
		}
	}
	// The recurring schedule is kept for its next slot; the one-shots are gone.
	waitFor(t, "only the recurring schedule left", func() bool { return store.count() == 1 })
	// The failed one-shot's count is gone with it; the cron's is kept.
	s.failMu.Lock()
	n := len(s.failing)
	s.failMu.Unlock()
	if n != 1 {
		t.Fatalf("failing counts kept = %d, want only the recurring schedule's", n)
	}
}

// A notify that fails outright is reported the same way; the scheduler never
// reports to itself.
func TestFailedScheduledNotifyIsReported(t *testing.T) {
	store := newFakeScheduleStore()
	del := &failingSchedDelivery{}
	events := &eventLog{}
	_, _ = store.CreateSchedule(context.Background(), "alpha", "down", "fyi", time.Now().Add(-time.Minute), "notify", nil, nil)
	_, _ = store.CreateSchedule(context.Background(), schedulerSender, "down", "loop?", time.Now().Add(-time.Minute), "notify", nil, nil)
	s := NewScheduler(store, del).WithEvents(events.add)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()
	waitFor(t, "both failures journaled", func() bool { return len(events.all()) == 2 })
	if got := del.reportsTo("alpha"); len(got) != 1 {
		t.Fatalf("reports to alpha = %+v", got)
	}
	if got := del.reportsTo(schedulerSender); len(got) != 0 {
		t.Fatalf("the scheduler reported to itself: %+v", got)
	}
}

// A schedule made on the dashboard (stored from "dashboard", a local surface
// that is no peer) is reported to its owner, the human.
func TestDashboardScheduleFailureGoesToTheHuman(t *testing.T) {
	store := newFakeScheduleStore()
	del := &failingSchedDelivery{}
	_, _ = store.CreateSchedule(context.Background(), "dashboard", "down", "check?", time.Now().Add(-time.Minute), "ask", nil, nil)
	owner := func(from string) string {
		if from == "dashboard" {
			return "prass"
		}
		return from
	}
	s := NewScheduler(store, del).WithOwner(owner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()
	waitFor(t, "the human told", func() bool { return len(del.reportsTo("prass")) == 1 })
	if got := del.reportsTo("dashboard"); len(got) != 0 {
		t.Fatalf("reported to the surface, not its owner: %+v", got)
	}
}

// flakyAskDelivery fails scheduled asks while down, and records notifies.
type flakyAskDelivery struct {
	failingSchedDelivery
	down atomic.Bool
}

func (d *flakyAskDelivery) OpenScheduledAsk(_ context.Context, _, _, _ string, _ *string, _ string) (string, error) {
	if d.down.Load() {
		return "", errPeerDown
	}
	return "ask-1", nil
}

// An outage reports once, not once per slot: a */15 heartbeat to a peer down
// overnight would otherwise send ~32 identical notices. The slot that goes
// out again reports the recovery, once.
func TestAnOutageReportsOnceAndItsRecoveryOnce(t *testing.T) {
	store := newFakeScheduleStore()
	del := &flakyAskDelivery{}
	del.down.Store(true)
	events := &eventLog{}
	cron := "*/15 * * * *"
	sched, _ := store.CreateSchedule(context.Background(), "orchestrator", "worker", "heartbeat?", time.Now().Add(-time.Minute), "ask", nil, &cron)
	s := NewScheduler(store, del).WithEvents(events.add)
	ctx := context.Background()
	for slot := 0; slot < 3; slot++ {
		current, _ := store.GetSchedule(ctx, sched.ScheduleID)
		s.fire(ctx, current)
	}
	reports := del.reportsTo("orchestrator")
	if len(reports) != 1 || !strings.Contains(reports[0].Text, "stay quiet until it goes out again") {
		t.Fatalf("three failed slots gave %d reports: %+v", len(reports), reports)
	}
	del.down.Store(false)
	current, _ := store.GetSchedule(ctx, sched.ScheduleID)
	s.fire(ctx, current)
	s.fire(ctx, current) // a second good slot reports nothing more
	reports = del.reportsTo("orchestrator")
	if len(reports) != 2 || !strings.Contains(reports[1].Text, "delivered again after 3 failed slot(s)") {
		t.Fatalf("after recovery: %+v", reports)
	}
	var kinds []string
	for _, ev := range events.all() {
		kinds = append(kinds, ev["type"].(string))
	}
	if strings.Join(kinds, ",") != "schedule_failed,schedule_recovered" {
		t.Fatalf("events = %v", kinds)
	}
}
