package service

import (
	"context"
	"errors"
	"sync"
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
