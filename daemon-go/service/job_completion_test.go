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
