package routing

import (
	"context"
	"testing"
	"time"
)

func arrivalExample(trace string, at time.Time, target string, cands map[string]float64) Example {
	d := Decision{
		Schema: SchemaV1, Arm: ArmExplicit, DecidedAt: at.Format(time.RFC3339Nano),
		Sender:   &Candidate{PeerID: "s"},
		Declared: Declared{PeerID: &target, Resolution: ResolutionResolved}, Chosen: &Chosen{PeerID: target},
	}
	for id, age := range cands {
		a := age
		d.Candidates = append(d.Candidates, Candidate{PeerID: id, Access: AccessOK, LastSeenAgeS: &a})
	}
	return Example{TraceID: trace, Decision: d}
}

func TestHybridArrivalAndNovelty(t *testing.T) {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	history := []Example{
		arrivalExample("a0", base.Add(-time.Minute), "b", map[string]float64{"a": 5, "b": 5}),
		arrivalExample("a1", base, "a", map[string]float64{"a": 5, "b": 5}),
		arrivalExample("a2", base.Add(time.Minute), "a", map[string]float64{"a": 5, "b": 5}),
		// c and d appear for the first time as candidates; c is the target.
		arrivalExample("a3", base.Add(2*time.Minute), "c", map[string]float64{"a": 5, "b": 5, "c": 30, "d": 90}),
		// e has never been in an ask but was last seen an hour ago: not an arrival.
		arrivalExample("a4", base.Add(3*time.Minute), "a", map[string]float64{"a": 5, "b": 5, "e": 3600}),
	}
	arms := BaselineArms(history, DefaultArrivalSeconds)
	ctx := context.Background()
	run := func(name string, i int) RouterResult {
		return Run(ctx, arms[name], InputFrom(history[i].TraceID, history[i].Decision), time.Second)
	}
	// a3: two arrivals (c, d), both under 600s: uniform over them.
	got := run("hybrid-arrival", 3)
	if got.Fallback || !got.Uniform || len(got.Scores) != 2 {
		t.Fatalf("a3 hybrid-arrival = %+v, want a uniform pick over c and d", got)
	}
	// a4: e is not recent enough, so habit answers (a has two earlier votes).
	got = run("hybrid-arrival", 4)
	if got.Fallback || got.Uniform || got.Scores[0].PeerID != "a" {
		t.Fatalf("a4 hybrid-arrival = %+v, want majority-past picking a", got)
	}
	// novelty has no age bound, so it does see e at a4.
	if got = run("novelty", 4); got.Fallback || len(got.Scores) != 1 || got.Scores[0].PeerID != "e" {
		t.Fatalf("a4 novelty = %+v, want e alone", got)
	}
	// Scored by expectation: a3's target is one of two arrivals, so 0.5.
	rep, rows := Replay(ctx, history[3:4], "hybrid-arrival", func(ctx context.Context, in Input) RouterResult {
		return Run(ctx, arms["hybrid-arrival"], in, time.Second)
	}, Options{})
	if rep.Scored != 1 || rep.Hit1 != 0 || rep.Fidelity1 != 0.5 || rep.Fidelity3 != 1 || rows[0].ExpectedHit1 != 0.5 {
		t.Fatalf("expected scoring: report %+v rows %+v", rep, rows)
	}
	// The arrival window is the knob: at 20s, c (30s) and d (90s) are no longer arrivals.
	tight := BaselineArms(history, 20)["hybrid-arrival"]
	if got = Run(ctx, tight, InputFrom("a3", history[3].Decision), time.Second); got.Uniform {
		t.Fatalf("20s window still fired: %+v", got)
	}
}

// When the snapshot carries prior_asks_total it decides "never asked", the
// ledger is only the fallback, and the row says which one spoke.
func TestNeverAskedPrefersTheSnapshotField(t *testing.T) {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	history := []Example{arrivalExample("h1", base, "a", map[string]float64{"a": 5, "b": 5})}
	scored := arrivalExample("s1", base.Add(time.Minute), "b", map[string]float64{"a": 5, "b": 30})
	// set records the snapshot's counts for id: nil means no field (ledger).
	set := func(id string, n *int) {
		for i := range scored.Decision.Candidates {
			if scored.Decision.Candidates[i].PeerID == id {
				scored.Decision.Candidates[i].PriorAsksTotal = n
				scored.Decision.Candidates[i].PriorAsksSent = n // nil stays absent; a value is the count asked and sent
			}
		}
	}
	zero, five := 0, 5
	arm := BaselineArms(history, DefaultArrivalSeconds)["hybrid-arrival"]
	ctx := context.Background()

	// Field on both: a has 5 prior asks, b has none, though the ledger knows only a.
	set("a", &five)
	set("b", &zero)
	got := Run(ctx, arm, InputFrom("s1", scored.Decision), time.Second)
	if !got.Uniform || len(got.Scores) != 1 || got.Scores[0].PeerID != "b" || got.NeverAskedSource != NeverAskedField {
		t.Fatalf("field on both = %+v", got)
	}
	// The field overrides the ledger: a is "never asked" by the field even though history saw it.
	set("a", &zero)
	set("b", &five)
	got = Run(ctx, arm, InputFrom("s1", scored.Decision), time.Second)
	if !got.Uniform || got.Scores[0].PeerID != "a" || got.NeverAskedSource != NeverAskedField {
		t.Fatalf("field override = %+v", got)
	}
	// Field on one candidate only: mixed, with the ledger judging the other.
	set("a", &five)
	set("b", nil)
	got = Run(ctx, arm, InputFrom("s1", scored.Decision), time.Second)
	if got.NeverAskedSource != NeverAskedMixed {
		t.Fatalf("mixed source = %q", got.NeverAskedSource)
	}
	// No field anywhere: ledger, and the habit path still reports it.
	set("a", nil)
	got = Run(ctx, arm, InputFrom("s1", scored.Decision), time.Second)
	if got.NeverAskedSource != NeverAskedLedger {
		t.Fatalf("ledger source = %q (%+v)", got.NeverAskedSource, got)
	}
}

// total+sent decides: a peer addressed zero times but that has sent asks has
// been in the room, so it is not an arrival.
func TestNeverAskedCountsAsksTheCandidateSent(t *testing.T) {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	ex := arrivalExample("s1", base, "b", map[string]float64{"a": 5, "b": 30})
	zero, three := 0, 3
	for i := range ex.Decision.Candidates {
		c := &ex.Decision.Candidates[i]
		c.PriorAsksTotal = &zero
		c.PriorAsksSent = &zero
		if c.PeerID == "a" {
			c.PriorAsksSent = &three // a never received an ask but has sent three
		}
	}
	got := Run(context.Background(), BaselineArms(nil, DefaultArrivalSeconds)["hybrid-arrival"], InputFrom("s1", ex.Decision), time.Second)
	if !got.Uniform || len(got.Scores) != 1 || got.Scores[0].PeerID != "b" || got.NeverAskedSource != NeverAskedField {
		t.Fatalf("arrivals = %+v, want only b", got)
	}
}
