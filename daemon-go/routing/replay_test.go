package routing

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
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
	rep, rows := Replay(context.Background(), examples, "stub", runner, Options{})
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
	rep, _ := Replay(context.Background(), examples, "stub", runner, Options{})
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
	rep, _ := Replay(context.Background(), examples, "paid", runner, Options{Recorded: true})
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
		func(context.Context, Input) RouterResult { return res }, Options{})
	if rep.Hit1 != 0 || rep.Hit3 != 0 || rep.Fallbacks != 1 || rows[0].Hit3 || rep.AnsweredFidelity1 != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestReplaySkipsTextArmsWhenTextNotRecorded(t *testing.T) {
	ex := example("ask-1", "repow-b", ArmExplicit, nil)
	ex.Decision.Request.Text, ex.Decision.Request.TextRecorded = "", false
	rep, _ := Replay(context.Background(), []Example{ex}, "jev-both", func(context.Context, Input) RouterResult { return ranked("repow-b") }, Options{})
	if rep.Skipped[SkipTextNotRecorded] != 1 {
		t.Fatalf("report = %+v", rep)
	}
	rep, _ = Replay(context.Background(), []Example{ex}, "random", func(context.Context, Input) RouterResult { return ranked("repow-b") }, Options{})
	if rep.Scored != 1 {
		t.Fatalf("random needs no text: %+v", rep)
	}
}

// fakeLedger serves trace rows by trace id.
type fakeLedger map[string][]state.TraceRow

func (f fakeLedger) TracesWithStage(_ context.Context, stage string, _ time.Time) ([]state.TraceRow, error) {
	var out []state.TraceRow
	for _, rows := range f {
		for _, r := range rows {
			if r.Stage == stage {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (f fakeLedger) StagesFor(_ context.Context, id string) ([]state.TraceRow, error) {
	return f[id], nil
}

func trace(id string, d Decision, stages ...string) []state.TraceRow {
	rows := []state.TraceRow{{TraceID: id, Stage: Stage, Detail: d.Detail()}}
	for _, st := range stages {
		row := state.TraceRow{TraceID: id, Stage: st, Detail: map[string]any{}}
		switch st {
		case "closed:send_failed":
			row.Stage, row.Detail["close_reason"] = "closed", "send_failed"
		case "closed:ack":
			row.Stage, row.Detail["close_reason"] = "closed", "ack"
		case "route_failed:quiesced":
			row.Stage, row.Detail["reason"] = "route_failed", "quiesced"
		}
		rows = append(rows, row)
	}
	return rows
}

// Delivery is called delivered only on positive evidence; a decision with no
// later stages, or a delivery still in flight, stays unknown.
func TestLoadExamplesRequiresEvidenceOfDelivery(t *testing.T) {
	d := example("x", "repow-b", ArmExplicit, nil).Decision
	cases := map[string][]string{
		"decision only":     nil,
		"in progress":       {"created", "resolved_peer", "routed"},
		"accepted":          {"created", "resolved_peer", "routed", "thread_input_accepted"},
		"socket only":       {"created", "resolved_peer", "routed", "websocket_sent"},
		"closed by peer":    {"created", "routed", "websocket_sent", "acked", "closed:ack"},
		"rejected":          {"created", "routed", "hook_received", "injection_failed", "closed:send_failed"},
		"no connection":     {"created", "routed", "no_connection", "closed:send_failed"},
		"quiesced":          {"route_failed:quiesced"},
		"send_failed close": {"created", "routed", "closed:send_failed"},
	}
	want := map[string]string{
		"decision only": DeliveryUnknown, "in progress": DeliveryUnknown, "accepted": DeliveryDelivered,
		"socket only": DeliverySent, "closed by peer": DeliveryDelivered, "rejected": DeliverySendFailed,
		"no connection": DeliveryNoConnection, "quiesced": DeliveryQuiesced, "send_failed close": DeliverySendFailed,
	}
	ledger := fakeLedger{}
	for name, stages := range cases {
		ledger[name] = trace(name, d, stages...)
	}
	examples, err := LoadExamples(context.Background(), ledger, time.Time{})
	if err != nil || len(examples) != len(cases) {
		t.Fatalf("examples = %d, %v", len(examples), err)
	}
	for _, ex := range examples {
		if ex.Outcome.Delivery != want[ex.TraceID] {
			t.Errorf("%s: delivery = %q, want %q", ex.TraceID, ex.Outcome.Delivery, want[ex.TraceID])
		}
	}
	rep, _ := Replay(context.Background(), examples, "stub", func(context.Context, Input) RouterResult { return ranked("repow-b") }, Options{})
	if c := rep.Cohorts[DeliveryUnknown]; c == nil || c.Scored != 2 {
		t.Fatalf("unknown deliveries must form their own cohort: %+v", rep.Cohorts)
	}
	if c := rep.Cohorts[DeliveryDelivered]; c == nil || c.Scored != 2 {
		t.Fatalf("delivered cohort = %+v", rep.Cohorts[DeliveryDelivered])
	}
}

// A planned arm skipped at planning time (saturated pool) is carried on the
// decision, not a row, and replays as planned_not_run:saturated.
func TestLoadExamplesReadsSkipsFromDecision(t *testing.T) {
	d := planned(example("x", "repow-b", ArmExplicit, nil), "paid").Decision
	d.ShadowSkipped = []RouterResult{{Arm: "paid", Fallback: true, FallbackReason: FallbackSaturated, Model: "m", Scores: []Score{}}}
	examples, _ := LoadExamples(context.Background(), fakeLedger{"ask-1": trace("ask-1", d, "thread_input_accepted")}, time.Time{})
	rep, _ := Replay(context.Background(), examples, "paid", nil, Options{Recorded: true})
	if rep.Skipped["planned_not_run:saturated"] != 1 {
		t.Fatalf("skipped = %v", rep.Skipped)
	}
}
