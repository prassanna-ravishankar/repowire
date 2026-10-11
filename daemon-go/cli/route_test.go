package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
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

// A masked run must say so in the table, and the recency split must print.
func TestReplayTableNamesMaskAndRecencyStrata(t *testing.T) {
	var out strings.Builder
	printReplayTable(&out, []routing.Report{{
		Arm: "jev-attested", Source: "rerun", Decisions: 1, Scored: 1, Mask: []string{"last_exchange_with_sender_at", "open_asks_inbound"},
		Skipped: map[string]int{}, Cohorts: map[string]*routing.Cohort{},
		Recency: map[string]*routing.Cohort{routing.StratumOtherContact: {Scored: 1}},
	}})
	text := out.String()
	for _, want := range []string{"masked (jev-attested, re-run): last_exchange_with_sender_at,open_asks_inbound", "recency declared_other"} {
		if !strings.Contains(text, want) {
			t.Fatalf("table missing %q:\n%s", want, text)
		}
	}
}

// The table discloses a history baseline's vote population and never prints
// fallback details.
func TestReplayTableShowsHistoryNotDetails(t *testing.T) {
	var out strings.Builder
	printReplayTable(&out, []routing.Report{{
		Arm: "majority-past", Source: "rerun", Decisions: 1, Scored: 1, Fallbacks: 1,
		Skipped: map[string]int{}, Cohorts: map[string]*routing.Cohort{},
		History:         &routing.History{Asks: 9, From: "2026-10-11T17:01:09Z", To: "2026-10-11T17:26:23Z"},
		FallbackDetails: map[string]map[string]int{"request_error": {"request_error: jev status 422: secret-ish body": 1}},
	}})
	text := out.String()
	if !strings.Contains(text, "history (majority-past votes from): 9 labelled asks, 2026-10-11T17:01:09Z to 2026-10-11T17:26:23Z") {
		t.Fatalf("history not disclosed:\n%s", text)
	}
	if strings.Contains(text, "secret-ish body") {
		t.Fatalf("table printed a fallback detail:\n%s", text)
	}
}

func TestArmNamesListBaselinesFirst(t *testing.T) {
	names := armNames(config.RouterConfig{})
	if len(names) < 3 || names[0] != "random" || names[1] != "recent" || names[2] != "majority" || slices.Contains(names[3:], "random") {
		t.Fatalf("arm names = %v", names)
	}
}
