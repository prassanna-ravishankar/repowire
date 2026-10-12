package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/state"
)

const routeReplayUsage = "route replay --arm ARM [--recorded] [--mask FIELD,...] [--since RFC3339] [--limit N] [--db PATH] [--json] [--rows]"

const routeDecisionBenchUsage = "route decisionbench --cards DIR --delegations FILE --orchestrator MODEL --arm ARM [--mask FIELD,...] [--limit N] [--json] [--rows]"

// runRoute shows the routing config (show) and replays routing decisions
// offline: recorded mesh asks (replay), or DecisionBench delegations as a
// stateless sanity check (decisionbench).
func runRoute(argv []string) int {
	if len(argv) == 0 {
		return usage(routeReplayUsage)
	}
	switch argv[0] {
	case "show":
		return runRouteShow(os.Stdout)
	case "replay":
		return runRouteReplay(parse(argv[1:], "recorded", "json", "rows"))
	case "decisionbench":
		return runRouteDecisionBench(parse(argv[1:], "json", "rows"))
	}
	return usage(routeReplayUsage)
}

// runRouteShow prints the routing config as config.Load resolves it, with
// the warnings the daemon would log. API keys are reported as set or not.
func runRouteShow(w io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		return fatal(err)
	}
	printRouting(w, cfg)
	return 0
}

func printRouting(w io.Writer, cfg config.Config) {
	r := cfg.Routing
	keyState := func(m config.RouterModel) string {
		if m.APIKey == "" {
			return "no key"
		}
		return "key set"
	}
	shadows := "none"
	if len(r.ShadowArms) > 0 {
		var parts []string
		for _, arm := range r.ShadowArms {
			n := r.SampleEvery[arm]
			if n == 0 {
				n = 1
			}
			parts = append(parts, fmt.Sprintf("%s (every %d)", arm, n))
		}
		shadows = strings.Join(parts, ", ")
	}
	fmt.Fprintf(w, "strategy     %s\n", r.Strategy)
	fmt.Fprintf(w, "record       %t (ask text %t)\n", r.Record, r.RecordText)
	fmt.Fprintf(w, "shadow arms  %s\n", shadows)
	fmt.Fprintf(w, "timeout      %gs\n", r.TimeoutSeconds)
	fmt.Fprintf(w, "jev          %s, %s\n", r.Jev.Model, keyState(r.Jev))
	fmt.Fprintf(w, "frontier     %s, %s\n", r.Frontier.Model, keyState(r.Frontier))
	for _, warning := range cfg.Warnings {
		fmt.Fprintf(w, "WARNING %s\n", warning)
	}
	fmt.Fprintln(w, "(the daemon reads this at startup; restart it to apply changes)")
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
	// The whole ledger is the history baselines vote from; --since (and
	// --limit, in replayArms) only narrow the rows scored.
	history, err := routing.LoadExamples(context.Background(), store, time.Time{})
	if err != nil {
		return fatal(err)
	}
	examples := history
	if !since.IsZero() {
		if examples, err = routing.LoadExamples(context.Background(), store, since); err != nil {
			return fatal(err)
		}
	}
	return replayArms(a, examples, history, a.bool("recorded"), routeReplayUsage)
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
	return replayArms(a, examples, examples, false, routeDecisionBenchUsage)
}

// replayArms runs --arm (or every arm for "all") over examples and prints the
// report table, --json reports, or --rows per-decision lines. Baselines
// (random, recent, majority) are computed locally and always re-run, so
// they sit beside recorded arms at no cost.
func replayArms(a args, examples, history []routing.Example, recorded bool, usageText string) int {
	arm := a.string("arm", "")
	if arm == "" {
		return usage(usageText)
	}
	mask, err := routing.ParseMask(a.string("mask", ""))
	if err != nil {
		return fatal(fmt.Errorf("route: %w", err))
	}
	if recorded && len(mask) > 0 {
		return fatal(fmt.Errorf("route: --mask applies to re-runs; recorded results were scored unmasked"))
	}
	cfg, err := config.Load()
	if err != nil {
		return fatal(err)
	}
	router, err := routing.NewRouter(cfg.Routing, nil)
	if err != nil {
		return fatal(err)
	}
	known := armNames(cfg.Routing)
	arms := []string{arm}
	if arm == "all" {
		arms = known
	} else if !slices.Contains(known, arm) {
		return fatal(fmt.Errorf("route: unknown arm %q (known: all, %v)", arm, known))
	}
	if n := a.integer("limit", 0); n > 0 && n < len(examples) {
		examples = examples[len(examples)-n:]
	}
	baselines := routing.BaselineArms(history)
	ctx := context.Background()
	reports := []routing.Report{}
	enc := json.NewEncoder(os.Stdout)
	for _, name := range arms {
		runner := func(ctx context.Context, in routing.Input) routing.RouterResult { return router.Run(ctx, name, in) }
		opts := routing.Options{Recorded: recorded, Mask: mask}
		if baseline, ok := baselines[name]; ok {
			runner = func(ctx context.Context, in routing.Input) routing.RouterResult {
				return routing.Run(ctx, baseline, in, router.Timeout())
			}
			opts.Recorded = false
		} else if !recorded {
			fmt.Fprintf(os.Stderr, "re-running %s on up to %d recorded asks; each is one API call\n", name, len(examples))
		}
		report, rows := routing.Replay(ctx, examples, name, runner, opts)
		if m, ok := baselines[name].(routing.Majority); ok {
			h := m.History()
			report.History = &h
		}
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
	for _, rep := range reports {
		if len(rep.Mask) > 0 {
			fmt.Fprintf(w, "masked (%s, re-run): %s\n", rep.Arm, strings.Join(rep.Mask, ","))
		}
	}
	for _, rep := range reports {
		if h := rep.History; h != nil {
			fmt.Fprintf(w, "history (%s votes from): %d labelled asks, %s to %s\n", rep.Arm, h.Asks, h.From, h.To)
		}
	}
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
	printSplit(w, "cohort", reports, func(rep routing.Report) map[string]*routing.Cohort { return rep.Cohorts })
	printSplit(w, "recency", reports, func(rep routing.Report) map[string]*routing.Cohort { return rep.Recency })
}

// printSplit prints one block per subset name found in any report.
func printSplit(w io.Writer, label string, reports []routing.Report, split func(routing.Report) map[string]*routing.Cohort) {
	seen := map[string]bool{}
	for _, rep := range reports {
		for name := range split(rep) {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "\n%s %s\n", label, name)
		for _, rep := range reports {
			if c := split(rep)[name]; c != nil {
				fmt.Fprintf(w, "  %-18s scored %d, fallbacks %d, hit@1 %d, hit@3 %d\n", rep.Arm, c.Scored, c.Fallbacks, c.Hit1, c.Hit3)
			}
		}
	}
}

// armNames lists the replay baselines, then every configured arm.
func armNames(cfg config.RouterConfig) []string {
	names := []string{}
	for name := range routing.Arms(cfg) {
		if !routing.IsBaseline(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return append(slices.Clone(routing.Baselines), names...)
}
