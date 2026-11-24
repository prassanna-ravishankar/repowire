package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// Blocked work is not dispatchable, so it must give the runner no deadline: with
// only blocked work present the loop parks until woken (no periodic re-tick),
// and Stop returns promptly (#458).
func TestJobRunnerParksWithOnlyBlockedWork(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: "blocked"}); err != nil {
		t.Fatal(err)
	}

	r := NewJobRunner(store, nil, nil)
	if d := r.nextDeadline(ctx); d != nil {
		t.Fatalf("deadline with only blocked work = %v, want nil (park until woken)", *d)
	}

	r.Start(ctx)
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return with only blocked work present")
	}
	got, _ := store.GetWork(ctx, work.WorkID)
	if got.State != "blocked" {
		t.Fatalf("blocked work state = %q after a runner pass, want blocked", got.State)
	}
}

// TestJobRunnerDeadlineIsTheStoredInstant (#429): the runner waits for the
// stored due_at / next_due_at itself, not for now + a delay measured against
// an earlier reading of the clock.
func TestJobRunnerDeadlineIsTheStoredInstant(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	due := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	if _, err := store.CreateWork(ctx, state.WorkCreate{Title: "later", Request: map[string]any{
		"execution": map[string]any{"schedule": map[string]any{"due_at": due.Format(time.RFC3339Nano)}},
	}}); err != nil {
		t.Fatal(err)
	}
	calendarDue := due.Add(-30 * time.Minute)
	if _, err := store.CreateCalendarEntry(ctx, &state.CalendarEntry{Title: "sooner", Kind: "general", Cron: "* * * * *",
		NextDueAt: calendarDue.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	r := NewJobRunner(store, nil, nil)
	// A clock that has moved on since the store was read must not shift the target.
	r.clock = wallClock{now: func() time.Time { return time.Now().Add(10 * time.Minute) }, chunk: time.Second}
	got := r.nextDeadline(ctx)
	if got == nil || !got.Equal(calendarDue) {
		t.Fatalf("deadline = %v, want the stored %v", got, calendarDue)
	}
}

// TestJobRunnerWakesForAJobDeadline (#457): a job's deadline_at is one of the
// instants the runner waits for, so a running job that passes it is expired
// on time, without a poll or a timer of its own.
func TestJobRunnerWakesForAJobDeadline(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadline := time.Now().UTC().Add(300 * time.Millisecond).Format(time.RFC3339Nano)
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "slow", DeadlineAt: &deadline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: "running"}); err != nil {
		t.Fatal(err)
	}
	r := NewJobRunner(store, nil, nil)
	r.OnDeadline(NewJobCompletion(store, nil, nil, nil, nil).ExpireOverdue)
	if got := r.nextDeadline(ctx); got == nil || got.Format(time.RFC3339Nano) != deadline {
		t.Fatalf("next deadline = %v, want the job's %s", got, deadline)
	}
	r.Start(ctx)
	defer r.Stop()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if got, _ := store.GetWork(ctx, work.WorkID); got.State == "expired" {
			return
		}
	}
	got, _ := store.GetWork(ctx, work.WorkID)
	t.Fatalf("job past its deadline = %s; want expired", got.State)
}

// Review #563: a run or retry from outside the loop (the HTTP run/retry
// routes) wakes it, so the dispatched attempt's deadline is waited for even
// when the runner was idle with nothing to wait for.
func TestRunJobWakesTheRunner(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewJobRunner(store, nil, nil)
	if _, err := r.RunJob(ctx, work.WorkID, true, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.wake:
	default:
		t.Fatal("RunJob did not wake the runner")
	}
}
