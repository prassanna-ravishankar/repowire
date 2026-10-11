package routing

import (
	"context"
	"testing"
)

func example(id, declared string, arm string, shadow map[string]RouterResult) Example {
	in := testInput()
	d := Decision{
		Schema: SchemaV1, Surface: SurfaceAsk, Arm: arm, Request: in.Request, Sender: in.Sender,
		Candidates: in.Candidates, DecidedAt: "2026-10-11T10:05:00Z",
	}
	if declared != "" {
		d.Declared = Declared{Raw: &declared, PeerID: &declared, Resolution: ResolutionResolved}
		d.Chosen = &Chosen{PeerID: declared}
	} else {
		d.Declared = Declared{Resolution: ResolutionAbsent}
	}
	return Example{TraceID: id, Decision: d, Shadow: shadow}
}

func ranked(ids ...string) RouterResult {
	res := RouterResult{Arm: "stub"}
	for i, id := range ids {
		res.Scores = append(res.Scores, Score{PeerID: id, Score: rankScore(i, len(ids))})
	}
	return res
}

func TestReplayScoresFidelityAgainstDeclaredTarget(t *testing.T) {
	examples := []Example{
		example("ask-1", "repow-b", ArmExplicit, nil),
		example("ask-2", "repow-c", ArmExplicit, nil),
		example("ask-3", "repow-zzz", ArmExplicit, nil), // declared peer not eligible
		example("ask-4", "", ArmExplicit, nil),          // nothing declared
		example("ask-5", "repow-b", "jev-both", nil),    // live-routed, no human label
	}
	runner := func(_ context.Context, in Input) RouterResult { return ranked("repow-c", "repow-b") }
	rep, rows := Replay(context.Background(), examples, "stub", runner, false)
	if rep.Scored != 2 || rep.Hit1 != 1 || rep.Hit3 != 2 || rep.Fidelity1 != 0.5 || rep.Fidelity3 != 1 {
		t.Fatalf("report = %+v", rep)
	}
	want := map[string]int{SkipDeclaredIneligible: 1, SkipNoDeclaredTarget: 1, SkipNotExplicit: 1}
	for reason, n := range want {
		if rep.Skipped[reason] != n {
			t.Fatalf("skipped = %v", rep.Skipped)
		}
	}
	if !rows[1].Hit1 || rows[0].Hit1 || !rows[0].Hit3 || rows[0].Top[0] != "repow-c" {
		t.Fatalf("rows = %+v", rows[:2])
	}
}

func TestReplayCountsFallbacksAsMisses(t *testing.T) {
	examples := []Example{example("ask-1", "repow-b", ArmExplicit, nil), example("ask-2", "repow-b", ArmExplicit, nil)}
	calls := 0
	runner := func(_ context.Context, in Input) RouterResult {
		calls++
		if calls == 1 {
			return RouterResult{Fallback: true, FallbackReason: FallbackTimeout}
		}
		return ranked("repow-b")
	}
	rep, _ := Replay(context.Background(), examples, "stub", runner, false)
	if rep.Fallbacks != 1 || rep.Fidelity1 != 0.5 || rep.AnsweredFidelity1 != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func planned(ex Example, arms ...string) Example {
	ex.Decision.ShadowPlanned = arms
	return ex
}

func TestReplayRecordedSeparatesUnsampledFromPlannedNotRun(t *testing.T) {
	examples := []Example{
		planned(example("ask-1", "repow-b", ArmExplicit, map[string]RouterResult{"paid": ranked("repow-b")}), "paid"),
		planned(example("ask-2", "repow-b", ArmExplicit, map[string]RouterResult{}), "paid"), // planned, row lost
		example("ask-3", "repow-b", ArmExplicit, map[string]RouterResult{}),                  // not sampled
		planned(example("ask-4", "repow-b", ArmExplicit, map[string]RouterResult{
			"paid": {Arm: "paid", Fallback: true, FallbackReason: FallbackSaturated, Scores: []Score{}},
		}), "paid"),
	}
	examples[0].Outcome.Delivery = DeliveryNoConnection
	runner := func(context.Context, Input) RouterResult {
		t.Fatal("recorded replay must not call the arm")
		return RouterResult{}
	}
	rep, _ := Replay(context.Background(), examples, "paid", runner, true)
	if rep.Source != "recorded" || rep.Scored != 1 || rep.Hit1 != 1 {
		t.Fatalf("report = %+v", rep)
	}
	want := map[string]int{SkipNotSampled: 1, "planned_not_run:missing": 1, "planned_not_run:saturated": 1}
	for reason, n := range want {
		if rep.Skipped[reason] != n {
			t.Fatalf("skipped = %v, want %v", rep.Skipped, want)
		}
	}
	if c := rep.Cohorts[DeliveryNoConnection]; c == nil || c.Scored != 1 || c.Hit1 != 1 {
		t.Fatalf("a failed delivery must be scored and reported as its own cohort: %+v", rep.Cohorts)
	}
}

// A fallback is a miss even if a future arm leaves scores on it.
func TestReplayFallbackWithScoresIsAMiss(t *testing.T) {
	res := ranked("repow-b")
	res.Fallback, res.FallbackReason = true, FallbackError
	rep, rows := Replay(context.Background(), []Example{example("ask-1", "repow-b", ArmExplicit, nil)}, "stub",
		func(context.Context, Input) RouterResult { return res }, false)
	if rep.Hit1 != 0 || rep.Hit3 != 0 || rep.Fallbacks != 1 || rows[0].Hit3 || rep.AnsweredFidelity1 != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestReplaySkipsTextArmsWhenTextNotRecorded(t *testing.T) {
	ex := example("ask-1", "repow-b", ArmExplicit, nil)
	ex.Decision.Request.Text, ex.Decision.Request.TextRecorded = "", false
	rep, _ := Replay(context.Background(), []Example{ex}, "jev-both", func(context.Context, Input) RouterResult { return ranked("repow-b") }, false)
	if rep.Skipped[SkipTextNotRecorded] != 1 {
		t.Fatalf("report = %+v", rep)
	}
	rep, _ = Replay(context.Background(), []Example{ex}, "random", func(context.Context, Input) RouterResult { return ranked("repow-b") }, false)
	if rep.Scored != 1 {
		t.Fatalf("random needs no text: %+v", rep)
	}
}
