package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/state"
)

const routeReplayUsage = "route replay --arm ARM [--recorded] [--since RFC3339] [--limit N] [--db PATH] [--json] [--rows]"

const routeDecisionBenchUsage = "route decisionbench --cards DIR --delegations FILE --orchestrator MODEL --arm ARM [--limit N] [--json] [--rows]"

// runRoute replays routing decisions offline: recorded mesh asks (replay), or
// DecisionBench delegations as a stateless sanity check (decisionbench).
func runRoute(argv []string) int {
	if len(argv) == 0 {
		return usage(routeReplayUsage)
	}
	switch argv[0] {
	case "replay":
		return runRouteReplay(parse(argv[1:], "recorded", "json", "rows"))
	case "decisionbench":
		return runRouteDecisionBench(parse(argv[1:], "json", "rows"))
	}
	return usage(routeReplayUsage)
}

// runRouteReplay scores arms against recorded asks. It reads state.db
// read-only, so it is safe beside a running daemon.
func runRouteReplay(a args) int {
	var since time.Time
	if raw := a.string("since", ""); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return fatal(fmt.Errorf("route replay: --since must be RFC-3339"))
		}
		since = parsed
	}
	store, err := state.OpenReadOnly(a.string("db", state.DefaultPath()))
	if err != nil {
		return fatal(err)
	}
	defer store.Close()
	examples, err := routing.LoadExamples(context.Background(), store, since)
	if err != nil {
		return fatal(err)
	}
	return replayArms(a, examples, a.bool("recorded"), routeReplayUsage)
}

// runRouteDecisionBench scores arms on DecisionBench delegations, read from
// files the caller downloaded (the dataset is not redistributed here).
func runRouteDecisionBench(a args) int {
	cardsDir, file, orchestrator := a.string("cards", ""), a.string("delegations", ""), a.string("orchestrator", "")
	if cardsDir == "" || file == "" || orchestrator == "" {
		return usage(routeDecisionBenchUsage)
	}
	cards, err := routing.LoadCards(cardsDir)
	if err != nil {
		return fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		return fatal(err)
	}
	defer f.Close()
	examples, err := routing.DelegationExamples(f, orchestrator, cards)
	if err != nil {
		return fatal(err)
	}
	return replayArms(a, examples, false, routeDecisionBenchUsage)
}

// replayArms runs --arm (or every arm for "all") over examples and prints the
// report table, --json reports, or --rows per-decision lines.
func replayArms(a args, examples []routing.Example, recorded bool, usageText string) int {
	arm := a.string("arm", "")
	if arm == "" {
		return usage(usageText)
	}
	cfg, err := config.Load()
	if err != nil {
		return fatal(err)
	}
	router, err := routing.NewRouter(cfg.Experiments.Router, nil)
	if err != nil {
		return fatal(err)
	}
	arms := []string{arm}
	if arm == "all" {
		arms = armNames(cfg.Experiments.Router)
	} else if _, known := routing.Arms(cfg.Experiments.Router)[arm]; !known {
		return fatal(fmt.Errorf("route: unknown arm %q (known: all, %v)", arm, armNames(cfg.Experiments.Router)))
	}
	if n := a.integer("limit", 0); n > 0 && n < len(examples) {
		examples = examples[len(examples)-n:]
	}
	ctx := context.Background()
	reports := []routing.Report{}
	enc := json.NewEncoder(os.Stdout)
	for _, name := range arms {
		if !recorded && name != "random" {
			fmt.Fprintf(os.Stderr, "re-running %s on up to %d recorded asks; each is one API call\n", name, len(examples))
		}
		report, rows := routing.Replay(ctx, examples, name, func(ctx context.Context, in routing.Input) routing.RouterResult {
			return router.Run(ctx, name, in)
		}, recorded)
		reports = append(reports, report)
		if a.bool("rows") {
			for _, row := range rows {
				_ = enc.Encode(struct {
					Arm string `json:"arm"`
					routing.Result
				}{name, row})
			}
		}
	}
	switch {
	case a.bool("rows"):
	case a.bool("json"):
		printJSON(reports)
	default:
		printReplayTable(os.Stdout, reports)
	}
	return 0
}

// printReplayTable renders one row per arm with a column for every skip
// reason present in any report, so no category of vanished observation is
// hidden (including per-arm not_sampled and planned_not_run:*).
func printReplayTable(w io.Writer, reports []routing.Report) {
	seen := map[string]bool{}
	reasons := []string{}
	for _, rep := range reports {
		for reason := range rep.Skipped {
			if !seen[reason] {
				seen[reason] = true
				reasons = append(reasons, reason)
			}
		}
	}
	sort.Strings(reasons)
	fmt.Fprintf(w, "%-18s %-8s %6s %6s %6s %6s %6s %8s %8s", "arm", "source", "rows", "scored", "fb", "f@1", "f@3", "ans@1", "ms")
	for _, r := range reasons {
		fmt.Fprintf(w, " %s", r)
	}
	fmt.Fprintln(w)
	for _, rep := range reports {
		fmt.Fprintf(w, "%-18s %-8s %6d %6d %6d %6.3f %6.3f %8.3f %8.0f", rep.Arm, rep.Source, rep.Decisions, rep.Scored, rep.Fallbacks, rep.Fidelity1, rep.Fidelity3, rep.AnsweredFidelity1, rep.MeanLatencyMS)
		for _, r := range reasons {
			fmt.Fprintf(w, " %*d", len(r), rep.Skipped[r])
		}
		fmt.Fprintln(w)
	}
	cohorts := map[string]bool{}
	for _, rep := range reports {
		for name := range rep.Cohorts {
			cohorts[name] = true
		}
	}
	names := make([]string, 0, len(cohorts))
	for name := range cohorts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "\ncohort %s\n", name)
		for _, rep := range reports {
			if c := rep.Cohorts[name]; c != nil {
				fmt.Fprintf(w, "  %-18s scored %d, fallbacks %d, hit@1 %d, hit@3 %d\n", rep.Arm, c.Scored, c.Fallbacks, c.Hit1, c.Hit3)
			}
		}
	}
}

func armNames(cfg config.RouterConfig) []string {
	names := []string{}
	for name := range routing.Arms(cfg) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
