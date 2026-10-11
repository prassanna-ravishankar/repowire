package cli

import (
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/routing"
)

// Every skip reason present must show in the human-readable table, including
// the planned-but-not-run categories, or observations vanish unexplained.
func TestReplayTableShowsEveryPresentSkipReason(t *testing.T) {
	var out strings.Builder
	printReplayTable(&out, []routing.Report{
		{Arm: "paid", Source: "recorded", Decisions: 3, Skipped: map[string]int{"planned_not_run:missing": 1, "planned_not_run:saturated": 1, "not_sampled": 1}, Cohorts: map[string]*routing.Cohort{}},
		{Arm: "random", Source: "rerun", Decisions: 3, Scored: 1, Skipped: map[string]int{"not_run:breaker_open": 2},
			Cohorts: map[string]*routing.Cohort{"unknown": {Scored: 1}}},
	})
	text := out.String()
	for _, want := range []string{"planned_not_run:missing", "planned_not_run:saturated", "not_sampled", "not_run:breaker_open", "cohort unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("table hides %q:\n%s", want, text)
		}
	}
}
