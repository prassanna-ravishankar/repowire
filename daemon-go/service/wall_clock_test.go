package service

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// jumpingClock reads the real clock plus an offset the test can move, standing
// in for a Mac that slept: the wall clock leaps while armed timers do not.
type jumpingClock struct{ offset atomic.Int64 }

func (j *jumpingClock) now() time.Time       { return time.Now().Add(time.Duration(j.offset.Load())) }
func (j *jumpingClock) jump(d time.Duration) { j.offset.Add(int64(d)) }

func TestWaitUntilNoticesWallClockJumpWithinOneChunk(t *testing.T) {
	jc := &jumpingClock{}
	clock := wallClock{now: jc.now, chunk: 20 * time.Millisecond}
	deadline := jc.now().Add(time.Hour)

	done := make(chan [2]bool, 1)
	go func() {
		woken, stopped := clock.waitUntil(context.Background(), deadline, nil, nil)
		done <- [2]bool{woken, stopped}
	}()
	time.Sleep(30 * time.Millisecond)
	jc.jump(2 * time.Hour) // the machine "slept" past the deadline
	select {
	case got := <-done:
		if got != [2]bool{false, false} {
			t.Fatalf("waitUntil = (woken %v, stopped %v), want deadline reached", got[0], got[1])
		}
	case <-time.After(time.Second):
		t.Fatal("wall-clock jump past the deadline not noticed within a second (chunk 20ms)")
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
// and then fell through to fire (repowire-m4s).
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
