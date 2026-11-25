package routing

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic cards in DecisionBench's documented shape; the real ones are not
// redistributed.
func writeCards(t *testing.T, models ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, m := range models {
		card := "---\nmodel: " + m + "\nvariant: c2_static\n---\n\n# " + m + "\n\n## Strengths\n- strong at " + m + " things\n"
		if err := os.WriteFile(filepath.Join(dir, m+".md"), []byte(card), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a card"), 0o600)
	return dir
}

func TestParseCard(t *testing.T) {
	c, err := ParseCard("---\nmodel: gpt-5.5\nflavor: c1_human\n---\n# body\n")
	if err != nil || c.Model != "gpt-5.5" || c.Body != "# body" {
		t.Fatalf("card = %+v, %v", c, err)
	}
	for _, bad := range []string{"# no frontmatter", "---\nmodel: x\n", "---\nflavor: c1\n---\nbody"} {
		if _, err := ParseCard(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestDelegationExamplesReplay(t *testing.T) {
	cards, err := LoadCards(writeCards(t, "alpha-model", "beta-model", "gamma-model"))
	if err != nil || len(cards) != 3 {
		t.Fatalf("cards = %d, %v", len(cards), err)
	}
	lines := strings.Join([]string{
		`{"task_id":"t1","peer":"beta-model","subtask":"compute the sum","output":"42"}`,
		`{"task_id":"t2","peer":"gamma-model","subtask":"look it up","error":"Timeout"}`,
		`{"task_id":"t3","peer":"unknown-model","subtask":"x","output":"y"}`,
		`{"task_id":"t4","peer":"gamma-model","subtask":"summarise","output":"ok"}`,
	}, "\n")
	examples, err := DelegationExamples(strings.NewReader(lines), "alpha-model", cards)
	if err != nil || len(examples) != 2 {
		t.Fatalf("examples = %d, %v (failed and uncarded delegations must be dropped)", len(examples), err)
	}
	d := examples[0].Decision
	if *d.Declared.PeerID != "beta-model" || len(d.Candidates) != 2 || d.Request.Text != "compute the sum" {
		t.Fatalf("decision = %+v", d)
	}
	for _, c := range d.Candidates {
		if c.PeerID == "alpha-model" {
			t.Fatal("the orchestrator cannot be a candidate")
		}
		if !strings.Contains(c.Description, "strong at") || c.TurnState != "" {
			t.Fatalf("candidate = %+v (claimed card only, no attested state)", c)
		}
	}
	runner := func(_ context.Context, in Input) RouterResult { return ranked("gamma-model", "beta-model") }
	rep, _ := Replay(context.Background(), examples, "stub", runner, Options{})
	if rep.Scored != 2 || rep.Hit1 != 1 || rep.Hit3 != 2 {
		t.Fatalf("report = %+v", rep)
	}
}

// The trace order is the clock (#462). Recency is per task, majority-past
// votes across the file. Tasks T1 = B B C and T2 = B C C from A, with D
// never chosen and a failed delegation inside T1, worked by hand:
//
//	row  task  declared  previous   votes so far  recent  majority-past  stratum
//	                     (in task)  (whole file)
//	1    T1    B         -          -             none    none           no_exchange_data
//	2    T1    B         B          B1            hit     hit            recent_contact
//	3    T1    C         B          B2            miss    miss           other
//	4    T2    B         -          B2 C1         none    hit            no_exchange_data
//	5    T2    C         B          B3 C1         miss    miss           other
//	6    T2    C         C          B3 C2         hit     miss           recent_contact
func TestDecisionBenchTraceOrderDrivesTheHistoryBaselines(t *testing.T) {
	cards, err := LoadCards(writeCards(t, "a", "b", "c", "d"))
	if err != nil {
		t.Fatal(err)
	}
	line := func(task, peer, failure string) string {
		if failure != "" {
			return `{"task_id":"` + task + `","peer":"` + peer + `","subtask":"s","error":"` + failure + `"}`
		}
		return `{"task_id":"` + task + `","peer":"` + peer + `","subtask":"s","output":"o"}`
	}
	lines := []string{
		line("T1", "b", ""), line("T1", "b", ""), line("T1", "d", "Timeout"), line("T1", "c", ""),
		line("T2", "b", ""), line("T2", "c", ""), line("T2", "c", ""),
	}
	examples, err := DelegationExamples(strings.NewReader(strings.Join(lines, "\n")), "a", cards)
	if err != nil || len(examples) != 6 {
		t.Fatalf("examples = %d, %v", len(examples), err)
	}

	// The clock strictly increases, and each candidate carries the last
	// delegation to it earlier in its task; earlier rows are not rewritten.
	var prev time.Time
	for i, ex := range examples {
		at, err := time.Parse(time.RFC3339Nano, ex.Decision.DecidedAt)
		if err != nil || !at.After(prev) {
			t.Fatalf("row %d decided_at %q after %v", i+1, ex.Decision.DecidedAt, prev)
		}
		prev = at
	}
	exchange := func(row int, peer string) string {
		for _, c := range examples[row-1].Decision.Candidates {
			if c.PeerID == peer && c.LastExchangeAt != nil {
				return *c.LastExchangeAt
			}
		}
		return ""
	}
	if exchange(1, "b") != "" || exchange(2, "b") != examples[0].Decision.DecidedAt || exchange(3, "d") != "" ||
		exchange(4, "b") != "" || exchange(4, "c") != "" || exchange(6, "c") != examples[4].Decision.DecidedAt {
		t.Fatalf("exchange stamps: row1 b=%q row2 b=%q row3 d=%q row4 b=%q row4 c=%q row6 c=%q (a new task starts with none)",
			exchange(1, "b"), exchange(2, "b"), exchange(3, "d"), exchange(4, "b"), exchange(4, "c"), exchange(6, "c"))
	}

	arms := BaselineArms(examples, 600)
	run := func(name string) Report {
		rep, _ := Replay(context.Background(), examples, name, func(ctx context.Context, in Input) RouterResult {
			return Run(ctx, arms[name], in, time.Second)
		}, Options{})
		return rep
	}
	recent, past := run("recent"), run("majority-past")
	if recent.Scored != 6 || recent.Fallbacks != 2 || recent.Hit1 != 2 {
		t.Fatalf("recent = scored %d, fallbacks %d, hit@1 %d; want 6, 2, 2", recent.Scored, recent.Fallbacks, recent.Hit1)
	}
	if past.Scored != 6 || past.Fallbacks != 1 || past.Hit1 != 2 {
		t.Fatalf("majority-past = scored %d, fallbacks %d, hit@1 %d; want 6, 1, 2", past.Scored, past.Fallbacks, past.Hit1)
	}
	strata := map[string]int{}
	for name, c := range recent.Recency {
		strata[name] = c.Scored
	}
	if want := map[string]int{StratumNoExchange: 2, StratumRecentContact: 2, StratumOtherContact: 2}; !maps.Equal(strata, want) {
		t.Fatalf("strata = %v, want %v", strata, want)
	}
	if recent.RepeatRate != 2.0/6 || past.RepeatRate != recent.RepeatRate {
		t.Fatalf("repeat rate = %v / %v, want 1/3", recent.RepeatRate, past.RepeatRate)
	}
}
