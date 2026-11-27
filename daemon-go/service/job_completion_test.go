package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

func TestJobCompletionArmsAndCompletesFromChatTurns(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "test job"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "attempt-0123456789ab"
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	status, phase, assigned := "delivered", "delivered", "repow-test-worker"
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &phase, AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}
	completion := NewJobCompletion(store, nil, nil, nil, nil)
	completion.OnChatTurn(ctx, proto.PeerID(assigned), "user", "job_id: "+work.WorkID+"\nattempt_id: "+attemptID)
	armed, _ := store.GetWork(ctx, work.WorkID)
	if armed.State != "running" || armed.Phase == nil || *armed.Phase != "turn_started" {
		t.Fatalf("job not armed: %#v", armed)
	}
	completion.OnChatTurn(ctx, proto.PeerID(assigned), "assistant", "finished successfully")
	completed, _ := store.GetWork(ctx, work.WorkID)
	if completed.State != "completed" || completed.ResultSummary == nil || *completed.ResultSummary != "finished successfully" {
		t.Fatalf("job not completed: %#v", completed)
	}
}

func TestJobCompletionFailsWorkWhenExecutorDies(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "terminal executor"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "attempt-abcdef012345"
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	status, phase, assigned := "delivered", "delivered", "repow-dead-worker"
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &phase, AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}

	completion := NewJobCompletion(store, nil, nil, nil, nil)
	completion.OnPeerTerminalOffline(proto.PeerID(assigned), "agent process exited")

	failed, err := store.GetWork(ctx, work.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != "failed" || failed.Phase == nil || *failed.Phase != "executor_died" {
		t.Fatalf("work not failed after executor death: %#v", failed)
	}
	if failed.Error["detail"] != "agent process exited" {
		t.Fatalf("terminal reason not preserved: %#v", failed.Error)
	}
}

// TestJobCompletionExpiresWorkPastItsDeadline (#457): deadline_at ends a job
// that is still going when it passes, whatever it is doing: a running attempt
// fails, the work becomes expired with deadline_exceeded, and its dispatch ask
// closes. Work not yet past its deadline, dispatching work (bounded by its
// lease) and terminal work are left alone.
func TestJobCompletionExpiresWorkPastItsDeadline(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	past, future := now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano)
	create := func(title string, deadline *string) *state.TrackedWork {
		w, err := store.CreateWork(ctx, state.WorkCreate{Title: title, DeadlineAt: deadline})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	asks := NewAskTracker(0)
	cid, err := asks.Register(ctx, RegisterAskParams{FromPeerID: "repow-jobs", FromPeerName: "jobs", ToPeerID: "repow-worker", ToPeerName: "worker", ReplyDelivery: "pull"})
	if err != nil {
		t.Fatal(err)
	}
	running := create("running", &past)
	attemptID := "attempt-0000deadline"
	if _, err := store.AcquireForDispatch(ctx, running.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	status, phase, assigned := "delivered", "delivered", "repow-worker"
	if _, err := store.UpdateAttempt(ctx, running.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &phase, AssignedPeerID: &assigned, CorrelationID: &cid}); err != nil {
		t.Fatal(err)
	}
	queued := create("queued", &past)
	later := create("later", &future)
	none := create("no deadline", nil)

	NewJobCompletion(store, asks, nil, nil, nil).ExpireOverdue(ctx, now)

	got, _ := store.GetWork(ctx, running.WorkID)
	if got.State != "expired" || got.StateReason == nil || *got.StateReason != "deadline_exceeded" {
		t.Fatalf("running work past its deadline = %s (%v)", got.State, got.StateReason)
	}
	if ask, _ := asks.Get(cid); !ask.Closed || ask.CloseReason != "deadline_exceeded" {
		t.Fatalf("dispatch ask = %+v; want closed deadline_exceeded", ask)
	}
	if got, _ := store.GetWork(ctx, queued.WorkID); got.State != "expired" {
		t.Fatalf("queued work past its deadline = %s", got.State)
	}
	for _, w := range []*state.TrackedWork{later, none} {
		if got, _ := store.GetWork(ctx, w.WorkID); got.State != "queued" {
			t.Fatalf("%s = %s; want untouched", w.Title, got.State)
		}
	}
}

// A daemon that stops between sending a job's prompt and recording it as
// delivered leaves the work dispatching; on restart it is recovered as
// unavailable with runner_interrupted. The agent still got the prompt, so its
// run arms and completes that attempt instead of being ignored.
func TestJobCompletionSettlesAnAttemptInterruptedByARestart(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "interrupted job"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "attempt-0a1b2c3d4e5f"
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	phase, assigned := "delivery", "repow-test-worker"
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Phase: &phase, AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}
	// The send went out; the daemon stopped before recording it. On restart
	// the runner lease has lapsed.
	if _, err := store.RecoverStaleDispatching(ctx, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	recovered, _ := store.GetWork(ctx, work.WorkID)
	if recovered.State != "unavailable" || recovered.StateReason == nil || *recovered.StateReason != "runner_interrupted" {
		t.Fatalf("work not recovered as runner_interrupted: %#v", recovered)
	}

	completion := NewJobCompletion(store, nil, nil, nil, nil)
	completion.OnChatTurn(ctx, proto.PeerID(assigned), "user", "job_id: "+work.WorkID+"\nattempt_id: "+attemptID)
	armed, _ := store.GetWork(ctx, work.WorkID)
	if armed.State != "running" || armed.Phase == nil || *armed.Phase != "turn_started" {
		t.Fatalf("interrupted attempt not armed by its run: %#v", armed)
	}
	completion.OnChatTurn(ctx, proto.PeerID(assigned), "assistant", "done after all")
	completed, _ := store.GetWork(ctx, work.WorkID)
	if completed.State != "completed" || completed.ResultSummary == nil || *completed.ResultSummary != "done after all" {
		t.Fatalf("interrupted attempt not completed by its run: %#v", completed)
	}
}

// Only the interrupted attempt's own executor can take it back, and only
// for that attempt: another peer's run, or a run naming another attempt,
// leaves it unavailable.
func TestJobCompletionLeavesAnInterruptedAttemptToItsOwnRun(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "interrupted job"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "attempt-5f4e3d2c1b0a"
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	phase, assigned := "delivery", "repow-test-worker"
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Phase: &phase, AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverStaleDispatching(ctx, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	completion := NewJobCompletion(store, nil, nil, nil, nil)
	completion.OnChatTurn(ctx, "repow-someone-else", "user", "job_id: "+work.WorkID+"\nattempt_id: "+attemptID)
	completion.OnChatTurn(ctx, proto.PeerID(assigned), "user", "job_id: "+work.WorkID+"\nattempt_id: attempt-ffffffffffff")
	if got, _ := store.GetWork(ctx, work.WorkID); got.State != "unavailable" {
		t.Fatalf("interrupted work taken by the wrong run: %#v", got)
	}
}
