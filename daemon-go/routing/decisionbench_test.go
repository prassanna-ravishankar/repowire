package routing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	rep, _ := Replay(context.Background(), examples, "stub", runner, false)
	if rep.Scored != 2 || rep.Hit1 != 1 || rep.Hit3 != 2 {
		t.Fatalf("report = %+v", rep)
	}
}
