package service

import (
	"context"
	"path/filepath"
	"testing"

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

// A hosted run reports its prompt and answer as the turns a hook runtime
// posts; a run that ends unanswered fails the job it carried.
func TestJobCompletionFromHostedRuns(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	completion := NewJobCompletion(store, nil, nil, nil, nil)
	deliver := func(title, attemptID, cid string) string {
		work, err := store.CreateWork(ctx, state.WorkCreate{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
			t.Fatal(err)
		}
		status, assigned := "delivered", "repow-default-h0st"
		if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &status, Phase: &status, AssignedPeerID: &assigned, CorrelationID: &cid}); err != nil {
			t.Fatal(err)
		}
		return work.WorkID
	}
	run := func(workID, attemptID, cid string) HostedDelivery {
		return HostedDelivery{Kind: "ask", CorrelationID: cid, ToPeerID: "repow-default-h0st", Text: "job_id: " + workID + "\nattempt_id: " + attemptID}
	}

	done := deliver("answered", "attempt-000000000001", "ask-done")
	completion.OnHostedRun(ctx, run(done, "attempt-000000000001", "ask-done"), HostedResult{Status: "done", Text: "all green"})
	if work, _ := store.GetWork(ctx, done); work.State != "completed" || work.ResultSummary == nil || *work.ResultSummary != "all green" {
		t.Fatalf("answered run did not complete the job: %#v", work)
	}

	lost := deliver("unanswered", "attempt-000000000002", "ask-lost")
	completion.OnHostedRun(ctx, run(lost, "attempt-000000000002", "ask-lost"), HostedResult{Status: "unanswered", Reason: "run_failed"})
	if work, _ := store.GetWork(ctx, lost); work.State != "failed" || work.Phase == nil || *work.Phase != "hosted_unanswered" || work.Error["detail"] != "run_failed" {
		t.Fatalf("unanswered run did not fail the job: %#v", work)
	}
}

// A replayed hosted run finishes the job wherever the last try stopped: after
// the attempt was written but not the work, and after the work was written
// but not what follows it.
func TestJobCompletionHostedRunIsReplaySafe(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	completion := NewJobCompletion(store, nil, nil, nil, nil)
	const attemptID, peerID = "attempt-000000000003", "repow-default-h0st"
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "replayed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: attemptID, IgnoreDueAt: true}); err != nil {
		t.Fatal(err)
	}
	delivered, assigned := "delivered", peerID
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &delivered, Phase: &delivered, AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}
	// The last try armed the work and wrote the attempt, then stopped.
	running, started := "running", "turn_started"
	if _, err := store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: running, Phase: &started, AttemptID: strPtr(attemptID)}); err != nil {
		t.Fatal(err)
	}
	completed := "completed"
	if _, err := store.UpdateAttempt(ctx, work.WorkID, attemptID, state.AttemptUpdate{Status: &completed, Phase: &completed}); err != nil {
		t.Fatal(err)
	}
	run := HostedDelivery{Kind: "ask", ToPeerID: peerID, Text: "job_id: " + work.WorkID + "\nattempt_id: " + attemptID}
	for try := 1; try <= 2; try++ {
		if err := completion.OnHostedRun(ctx, run, HostedResult{Status: "done", Text: "all green"}); err != nil {
			t.Fatalf("try %d: %v", try, err)
		}
		if got, _ := store.GetWork(ctx, work.WorkID); got.State != "completed" || got.ResultSummary == nil || *got.ResultSummary != "all green" {
			t.Fatalf("try %d did not leave the job completed: %#v", try, got)
		}
	}
}
