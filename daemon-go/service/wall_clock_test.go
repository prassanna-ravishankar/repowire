package service

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// syncBuffer is a log sink safe to write from the runner goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// jumpingClock reads the real clock plus an offset the test can move, standing
// in for a Mac that slept: the wall clock leaps while armed timers do not.
type jumpingClock struct{ offset atomic.Int64 }

func (j *jumpingClock) now() time.Time       { return time.Now().Add(time.Duration(j.offset.Load())) }
func (j *jumpingClock) jump(d time.Duration) { j.offset.Add(int64(d)) }

func TestWaitUntilNoticesForwardJumpWithinAboutOneChunk(t *testing.T) {
	const chunk = 20 * time.Millisecond
	jc := &jumpingClock{}
	clock := wallClock{now: jc.now, chunk: chunk}
	deadline := jc.now().Add(time.Hour)

	done := make(chan [2]bool, 1)
	go func() {
		woken, stopped := clock.waitUntil(context.Background(), deadline, nil, nil)
		done <- [2]bool{woken, stopped}
	}()
	time.Sleep(30 * time.Millisecond)
	jc.jump(2 * time.Hour) // the machine "slept" past the deadline
	jumped := time.Now()
	select {
	case got := <-done:
		if got != [2]bool{false, false} {
			t.Fatalf("waitUntil = (woken %v, stopped %v), want deadline reached", got[0], got[1])
		}
		// One chunk plus scheduling slack (race detector, loaded CI).
		if late := time.Since(jumped); late > chunk+60*time.Millisecond {
			t.Fatalf("jump noticed after %v; want within about one %v chunk", late, chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("wall-clock jump past the deadline not noticed")
	}
}

// TestWaitUntilBackwardJumpWaitsForOriginalInstant: a wall clock set back
// extends the wait to the original absolute deadline instead of firing on the
// elapsed duration.
func TestWaitUntilBackwardJumpWaitsForOriginalInstant(t *testing.T) {
	jc := &jumpingClock{}
	clock := wallClock{now: jc.now, chunk: 10 * time.Millisecond}
	deadline := jc.now().Add(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		clock.waitUntil(context.Background(), deadline, nil, nil)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	jc.jump(-time.Hour)
	select {
	case <-done:
		t.Fatal("returned on elapsed time after the wall clock was set back an hour")
	case <-time.After(150 * time.Millisecond):
	}
	jc.jump(time.Hour) // clock restored: the original instant has now passed
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("did not return once the wall clock passed the original deadline")
	}
}

func TestWaitUntilNeverReturnsBeforeWallDeadline(t *testing.T) {
	clock := wallClock{now: time.Now, chunk: 7 * time.Millisecond}
	deadline := time.Now().Add(60 * time.Millisecond)
	if woken, stopped := clock.waitUntil(context.Background(), deadline, nil, nil); woken || stopped {
		t.Fatalf("waitUntil = (woken %v, stopped %v), want deadline reached", woken, stopped)
	}
	if early := deadline.Sub(time.Now()); early > 0 {
		t.Fatalf("returned %v before the wall-clock deadline", early)
	}
}

func TestWaitUntilReturnsOnWakeAndStop(t *testing.T) {
	clock := wallClock{now: time.Now, chunk: time.Hour}
	deadline := time.Now().Add(time.Hour)

	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	if woken, stopped := clock.waitUntil(context.Background(), deadline, wake, nil); !woken || stopped {
		t.Fatalf("wake: got (woken %v, stopped %v)", woken, stopped)
	}
	stop := make(chan struct{})
	close(stop)
	if woken, stopped := clock.waitUntil(context.Background(), deadline, nil, stop); woken || !stopped {
		t.Fatalf("stop: got (woken %v, stopped %v)", woken, stopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if woken, stopped := clock.waitUntil(ctx, deadline, nil, nil); woken || !stopped {
		t.Fatalf("ctx: got (woken %v, stopped %v)", woken, stopped)
	}
}

// TestSchedulerFiresOverdueScheduleAfterSleep: a schedule that comes due while
// the machine sleeps fires within a chunk of waking, not after the full timer
// has accumulated awake time (repowire-2ph).
func TestSchedulerFiresOverdueScheduleAfterSleep(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	jc := &jumpingClock{}
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "overnight", time.Now().Add(time.Hour), "notify", nil, nil)

	s := NewScheduler(store, del)
	s.clock = wallClock{now: jc.now, chunk: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	time.Sleep(50 * time.Millisecond)
	if del.notifyCount() != 0 {
		t.Fatal("schedule fired before its fire_at")
	}
	jc.jump(2 * time.Hour)
	waitFor(t, "overdue schedule fires after the wall clock jumps", func() bool { return del.notifyCount() == 1 })
}

// TestSchedulerDoesNotFireFarScheduleEarly: a schedule further out than any
// single timer must wait for its fire_at. The old loop capped a sleep at 1h
// and then fell through to fire (repowire-m4s). This is a forward-looking
// regression test on the chunked loop: the old loop had no clock seam, so it
// was reproduced by shrinking its 1h cap locally, not by this test.
func TestSchedulerDoesNotFireFarScheduleEarly(t *testing.T) {
	store := newFakeScheduleStore()
	del := &fakeSchedDelivery{}
	_, _ = store.CreateSchedule(context.Background(), "alpha", "beta", "in five hours", time.Now().Add(5*time.Hour), "notify", nil, nil)

	s := NewScheduler(store, del)
	s.clock = wallClock{now: time.Now, chunk: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(150 * time.Millisecond) // ~30 chunks
	s.Stop()

	if n := del.notifyCount(); n != 0 {
		t.Fatalf("schedule 5h out fired %d times after 150ms of 5ms chunks", n)
	}
}

// TestJobRunnerDispatchesWhenDueAtArrives drives the runner loop through the
// wall-clock wait: a queued job is left alone until its due_at, then attempted.
func TestJobRunnerDispatchesWhenDueAtArrives(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dueAt := time.Now().UTC().Add(200 * time.Millisecond).Format(time.RFC3339Nano)
	work, err := store.CreateWork(ctx, state.WorkCreate{
		Title:   "due soon",
		Request: map[string]any{"execution": map[string]any{"schedule": map[string]any{"due_at": dueAt}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// No SessionControl: an attempt records missing_session_control, which is
	// enough to observe that dispatch happened.
	r := NewJobRunner(store, nil, nil)
	r.clock = wallClock{now: time.Now, chunk: 10 * time.Millisecond}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Start(runCtx)
	defer r.Stop()

	stillQueued := func() bool {
		got, _ := store.GetWork(ctx, work.WorkID)
		return got != nil && got.State == "queued"
	}
	time.Sleep(50 * time.Millisecond)
	if !stillQueued() {
		t.Fatal("job dispatched before its due_at")
	}
	waitFor(t, "job attempted once due_at passes", func() bool { return !stillQueued() })
}

func TestWaitUntilStopWinsOverPastDeadline(t *testing.T) {
	clock := wallClock{now: time.Now, chunk: time.Hour}
	past := time.Now().Add(-time.Minute)

	stop := make(chan struct{})
	close(stop)
	if woken, stopped := clock.waitUntil(context.Background(), past, nil, stop); woken || !stopped {
		t.Fatalf("closed stop + past deadline: got (woken %v, stopped %v), want stopped", woken, stopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if woken, stopped := clock.waitUntil(ctx, past, nil, nil); woken || !stopped {
		t.Fatalf("cancelled ctx + past deadline: got (woken %v, stopped %v), want stopped", woken, stopped)
	}
}

// newStuckCalendarRunner starts a JobRunner over an active calendar entry that
// is overdue and cannot advance, so every tick's MaterializeDue fails and the
// next deadline stays in the past. The cron is valid: Feb 29 lies beyond
// NextFireAfter's one-year search from 2026, which is how a person reaches
// this, not malformed config.
func newStuckCalendarRunner(t *testing.T) (*JobRunner, *syncBuffer) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.CreateCalendarEntry(context.Background(), &state.CalendarEntry{
		Title:     "stuck",
		Kind:      "general",
		Cron:      "0 0 29 2 *",
		NextDueAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r := NewJobRunner(store, nil, nil)
	r.Start(context.Background())
	return r, logs
}

// TestJobRunnerStopsWhileCalendarCannotAdvance: Stop must return promptly even
// when the runner's deadline is permanently in the past.
func TestJobRunnerStopsWhileCalendarCannotAdvance(t *testing.T) {
	r, _ := newStuckCalendarRunner(t)
	time.Sleep(30 * time.Millisecond)
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("JobRunner.Stop did not return within 200ms while a calendar entry could not advance")
	}
}

// TestJobRunnerDoesNotSpinOnStuckDeadline: a deadline the tick cannot advance
// is retried at most once per stuckDeadlineRetry. main spun ~4,400 ticks/s here,
// each logging and querying SQLite.
func TestJobRunnerDoesNotSpinOnStuckDeadline(t *testing.T) {
	r, logs := newStuckCalendarRunner(t)
	time.Sleep(300 * time.Millisecond)
	r.Stop()
	logs.mu.Lock()
	failures := strings.Count(logs.buf.String(), "materialize_due failed")
	logs.mu.Unlock()
	if failures > 2 {
		t.Fatalf("%d materialize_due failures in 300ms; a stuck deadline should retry at most once a second", failures)
	}
}

// TestJobRunnerMaterializesCalendarWhenDue: a recurring entry is materialized
// at its next_due_at through the wall-clock wait, not before.
func TestJobRunnerMaterializesCalendarWhenDue(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateCalendarEntry(ctx, &state.CalendarEntry{
		Title:     "recurring",
		Kind:      "general",
		Cron:      "* * * * *",
		NextDueAt: time.Now().UTC().Add(200 * time.Millisecond).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	r := NewJobRunner(store, nil, nil)
	r.clock = wallClock{now: time.Now, chunk: 10 * time.Millisecond}
	r.Start(ctx)
	defer r.Stop()

	occurrences := func() int {
		works, _ := store.ListWork(ctx, state.WorkFilter{})
		return len(works)
	}
	time.Sleep(50 * time.Millisecond)
	if n := occurrences(); n != 0 {
		t.Fatalf("%d occurrences before next_due_at", n)
	}
	waitFor(t, "occurrence materialized once due", func() bool { return occurrences() == 1 })
}

// TestJobRunnerRecoversLeaseWhenItExpires: a dispatching attempt is recovered
// at its lease expiry through the wall-clock wait, not before.
func TestJobRunnerRecoversLeaseWhenItExpires(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "leased"})
	if err != nil {
		t.Fatal(err)
	}
	lease := time.Now().UTC().Add(200 * time.Millisecond).Format("2006-01-02T15:04:05.000000-07:00")
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", LeaseUntil: lease, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	r := NewJobRunner(store, nil, nil)
	r.clock = wallClock{now: time.Now, chunk: 10 * time.Millisecond}
	r.Start(ctx)
	defer r.Stop()

	dispatching := func() bool {
		got, _ := store.GetWork(ctx, work.WorkID)
		return got != nil && got.State == "dispatching"
	}
	time.Sleep(50 * time.Millisecond)
	if !dispatching() {
		t.Fatal("lease recovered before it expired")
	}
	waitFor(t, "lease recovered once expired", func() bool { return !dispatching() })
}
