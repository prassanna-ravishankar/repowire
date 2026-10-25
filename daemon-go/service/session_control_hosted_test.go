package service

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// scHosted registers each spawned hosted agent in the fake registry, as the
// real Hosted.Spawn does.
type scHosted struct {
	reg     *scRegistry
	spawns  []HostedSpawnParams
	retired []proto.PeerID
}

func (h *scHosted) Spawn(_ context.Context, params HostedSpawnParams) (proto.PeerID, proto.DisplayName, error) {
	h.spawns = append(h.spawns, params)
	id := proto.PeerID("repow-default-h" + string(rune('0'+len(h.spawns))))
	h.reg.peers = append(h.reg.peers, &proto.Peer{PeerID: id, DisplayName: "job-hosted", Circle: params.Circle,
		Backend: proto.AgentHosted, Path: params.Path, Status: proto.StatusOnline})
	return id, "job-hosted", nil
}

func (h *scHosted) Retire(_ context.Context, id proto.PeerID) error {
	h.retired = append(h.retired, id)
	return nil
}

func TestSessionControlRunsJobsOnHostedAgents(t *testing.T) {
	ctx := context.Background()
	store := scStore(t)
	reg := &scRegistry{}
	hosted := &scHosted{reg: reg}
	control := NewSessionControl(reg, nil, store)
	control.hosted = hosted
	circle := "default"
	acquire := func(continuity string) *ExecutorAcquisition {
		t.Helper()
		work, err := store.CreateWork(ctx, state.WorkCreate{Title: "digest", Circle: &circle, Request: map[string]any{"execution": map[string]any{
			"target": map[string]any{"path": "/jobs/digest", "backend": string(proto.AgentHosted)}, "process_scope": "per_fire", "continuity": continuity,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if work, err = store.AcquireForDispatch(ctx, work.WorkID, state.AcquireOptions{RunnerOwnerID: "test", AttemptID: "attempt-a1b2c3d4e5f6", IgnoreDueAt: true}); err != nil {
			t.Fatal(err)
		}
		got, err := control.AcquireExecutorForWork(ctx, work, map[string]any{"path": "/jobs/digest", "backend": string(proto.AgentHosted)}, "test")
		if err != nil {
			t.Fatalf("acquire %s: %v", continuity, err)
		}
		return got
	}

	first, again := acquire("resume"), acquire("resume")
	if len(hosted.spawns) != 1 || first.Peer.PeerID != again.Peer.PeerID || first.ReleaseHandle != nil {
		t.Fatalf("resume did not reuse one agent: spawns=%+v release=%v", hosted.spawns, first.ReleaseHandle)
	}

	fresh := acquire("fresh")
	if len(hosted.spawns) != 2 || hosted.spawns[1].Path != "/jobs/digest-a1b2c3" || fresh.ReleaseHandle["kind"] != "hosted_agent" {
		t.Fatalf("fresh fire did not get its own agent: spawns=%+v release=%v", hosted.spawns, fresh.ReleaseHandle)
	}
	op, err := store.CreateOperation(ctx, "session.release_executor", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result, _ := control.releaseHosted(ctx, op.OperationID, fresh.ReleaseHandle, "completed"); result["status"] != "released" || len(hosted.retired) != 1 || hosted.retired[0] != fresh.Peer.PeerID {
		t.Fatalf("release = %v, retired %v", result, hosted.retired)
	}
}

// fastHostedOpener reports the hosted run from inside the send, as a runtime
// that settles before the send returns would.
type fastHostedOpener struct {
	completion *JobCompletion
	result     HostedResult
}

func (o *fastHostedOpener) OpenScheduledAsk(ctx context.Context, cid, _, toPeer, text string, _ *string, _ string) (string, error) {
	o.completion.OnHostedRun(ctx, HostedDelivery{Kind: "ask", CorrelationID: cid, ToPeerID: toPeer, Text: text}, o.result)
	return cid, nil
}

// A hosted run that reports before dispatch returns still completes or fails
// its job: dispatch records the ask id and delivered state before the send,
// and does not overwrite the outcome after it.
func TestJobDispatchSurvivesAHostedReportBeforeTheSendReturns(t *testing.T) {
	for _, tc := range []struct {
		result    HostedResult
		wantState string
	}{
		{HostedResult{Status: "done", Text: "all green"}, "completed"},
		{HostedResult{Status: "unanswered", Reason: "budget_exceeded"}, "failed"},
	} {
		t.Run(tc.wantState, func(t *testing.T) {
			ctx := context.Background()
			store := scStore(t)
			reg := &scRegistry{}
			control := NewSessionControl(reg, nil, store)
			control.hosted = &scHosted{reg: reg}
			opener := &fastHostedOpener{completion: NewJobCompletion(store, nil, nil, nil, nil), result: tc.result}
			runner := NewJobRunner(store, opener, control)
			circle := "default"
			work, err := store.CreateWork(ctx, state.WorkCreate{Title: "digest", Circle: &circle, Request: map[string]any{"execution": map[string]any{
				"target": map[string]any{"path": "/jobs/digest", "backend": string(proto.AgentHosted)}, "continuity": "resume",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.RunJob(ctx, work.WorkID, true, false); err != nil {
				t.Fatal(err)
			}
			got, err := store.GetWork(ctx, work.WorkID)
			if err != nil || got.State != tc.wantState {
				t.Fatalf("job state = %q, want %q (%v): %#v", got.State, tc.wantState, err, got)
			}
		})
	}
}
