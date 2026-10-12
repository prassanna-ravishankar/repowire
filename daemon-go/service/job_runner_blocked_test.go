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
	if d := r.secondsUntilNextDeadline(ctx); d != nil {
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
