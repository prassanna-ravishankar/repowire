package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/state"
)

const routeReplayUsage = "route replay --arm ARM [--recorded] [--mask FIELD,...] [--since RFC3339] [--limit N] [--db PATH | --input FILE] [--where KEY=VALUE,...] [--arrival-seconds N] [--allow-paid] [--json] [--rows]"

const routeReconstructUsage = "route reconstruct --db PATH --out FILE [--live-window DUR] [--exchange-ttl DUR] [--working-window DUR] [--json]"

const routeDecisionBenchUsage = "route decisionbench --cards DIR --delegations FILE --orchestrator MODEL --arm ARM [--mask FIELD,...] [--limit N] [--allow-paid] [--json] [--rows]"

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
		return runRouteReplay(parse(argv[1:], "recorded", "json", "rows", "allow-paid"))
	case "reconstruct":
		return runRouteReconstruct(parse(argv[1:], "json"))
	case "decisionbench":
		return runRouteDecisionBench(parse(argv[1:], "json", "rows", "allow-paid"))
	}
	return usage(routeReplayUsage)
}

// runRouteShow prints the routing config the daemon would run: the
// effective config after every degradation, then each setting that was
// ignored, then the warnings. API keys are reported as set or not.
func runRouteShow(w io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		return fatal(err)
	}
	router, warnings := routing.NewRouterOrStatic(cfg.Routing, nil)
	printRouting(w, router.Config(), cfg.Routing, append(cfg.Warnings, warnings...))
	return 0
}

// printRouting prints effective, then what requested asked for that
// effective does not run, then warnings (which also name settings dropped
// while loading, before requested was built).
func printRouting(w io.Writer, effective, requested config.RouterConfig, warnings []string) {
	keyState := func(m config.RouterModel) string {
		if m.APIKey == "" {
			return "no key"
		}
		return "key set"
	}
	clefState := func(c config.RouterClef) string {
		if c.AccountID == "" {
			return keyState(c.RouterModel) + ", no account id"
		}
		return keyState(c.RouterModel) + ", account id set"
	}
	shadows := "none"
	if len(effective.ShadowArms) > 0 {
		var parts []string
		for _, arm := range effective.ShadowArms {
			n := effective.SampleEvery[arm]
			if n == 0 {
				n = 1
			}
			parts = append(parts, fmt.Sprintf("%s (every %d)", arm, n))
		}
		shadows = strings.Join(parts, ", ")
	}
	fmt.Fprintln(w, "effective routing (what the daemon runs)")
	fmt.Fprintf(w, "  strategy     %s\n", effective.Strategy)
	fmt.Fprintf(w, "  record       %t (ask text %t)\n", effective.Record, effective.RecordText)
	fmt.Fprintf(w, "  shadow arms  %s\n", shadows)
	fmt.Fprintf(w, "  timeout      %gs\n", effective.TimeoutSeconds)
	fmt.Fprintf(w, "  service targets allowed  %t\n", effective.AllowServiceTargets)
	fmt.Fprintf(w, "  jev          %s, %s\n", effective.Jev.Model, keyState(effective.Jev))
	fmt.Fprintf(w, "  frontier     %s, %s\n", effective.Frontier.Model, keyState(effective.Frontier))
	fmt.Fprintf(w, "  clef         %s, %s, %s\n", effective.Clef.Model, effective.Clef.FlashModel, clefState(effective.Clef))
	var ignored []string
	if requested.Strategy != effective.Strategy {
		ignored = append(ignored, fmt.Sprintf("routing.strategy %q (running %s)", requested.Strategy, effective.Strategy))
	}
	for _, arm := range requested.ShadowArms {
		if !slices.Contains(effective.ShadowArms, arm) {
			ignored = append(ignored, fmt.Sprintf("routing.shadow_arms entry %q", arm))
		}
	}
	for arm := range requested.SampleEvery {
		if _, ok := effective.SampleEvery[arm]; !ok {
			ignored = append(ignored, fmt.Sprintf("routing.sample_every key %q", arm))
		}
	}
	sort.Strings(ignored)
	if len(ignored) > 0 {
		fmt.Fprintln(w, "IGNORED (configured but not running)")
		for _, line := range ignored {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	for _, warning := range warnings {
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
	if input := a.string("input", ""); input != "" {
		return runRouteReplayInput(a, input)
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

// runRouteReplayInput replays a dataset written by route reconstruct. There
// is no ledger behind it, so only the baselines are meaningful.
func runRouteReplayInput(a args, input string) int {
	if a.bool("recorded") {
		return fatal(fmt.Errorf("route replay: --recorded needs a ledger; --input rows carry no shadow results"))
	}
	f, err := os.Open(input)
	if err != nil {
		return fatal(err)
	}
	defer f.Close()
	history, err := routing.ReadExamples(f)
	if err != nil {
		return fatal(err)
	}
	examples := history
	if where := a.string("where", ""); where != "" {
		conds := map[string]string{}
		for _, part := range strings.Split(where, ",") {
			key, want, ok := strings.Cut(strings.TrimSpace(part), "=")
			if _, known := (routing.Strata{}).Get(key); !ok || !known {
				return fatal(fmt.Errorf("route replay: --where takes KEY=VALUE pairs over hub_bound, sender_type, recency (got %q)", part))
			}
			conds[key] = want
		}
		// Only the scored rows narrow; history baselines still vote from all.
		examples = slices.DeleteFunc(slices.Clone(history), func(ex routing.Example) bool {
			for key, want := range conds {
				if got, _ := ex.Strata.Get(key); got != want {
					return true
				}
			}
			return false
		})
	}
	return replayArms(a, examples, history, false, routeReplayUsage)
}

// runRouteReconstruct rebuilds routing inputs for asks that predate
// route_decision rows from the events journal, and writes them as JSONL for
// route replay --input. It opens the db read-only; point it at a copy anyway.
func runRouteReconstruct(a args) int {
	dbPath, out := a.string("db", ""), a.string("out", "")
	if dbPath == "" || out == "" {
		return usage(routeReconstructUsage)
	}
	opts := routing.ReconstructOptions{}
	for name, dst := range map[string]*time.Duration{"live-window": &opts.LiveWindow, "exchange-ttl": &opts.ExchangeTTL, "working-window": &opts.WorkingWindow} {
		if raw := a.string(name, ""); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return fatal(fmt.Errorf("route reconstruct: --%s must be a positive duration", name))
			}
			*dst = d
		}
	}
	store, err := state.OpenReadOnly(dbPath)
	if err != nil {
		return fatal(err)
	}
	defer store.Close()
	events, err := store.EventsOfTypes(context.Background(), routing.ReconstructTypes...)
	if err != nil {
		return fatal(err)
	}
	rows, summary := routing.Reconstruct(events, opts)
	f, err := os.Create(out)
	if err != nil {
		return fatal(err)
	}
	if err := routing.WriteRows(f, rows); err != nil {
		_ = f.Close()
		return fatal(err)
	}
	if err := f.Close(); err != nil {
		return fatal(err)
	}
	if a.bool("json") {
		printJSON(summary)
	} else {
		routing.WriteSummary(os.Stdout, summary)
	}
	return 0
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
// report table, --json reports, or --rows per-decision lines. Re-running
// a model arm is one paid provider call per row, so those arms need
// --allow-paid; without it "all" means every free arm, and a named paid arm
// is refused. Call counts print on exit, including after Ctrl-C. Baselines
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
	if !a.bool("allow-paid") {
		var skipped []string
		free := slices.DeleteFunc(slices.Clone(arms), func(name string) bool {
			paid := !recorded && !routing.IsBaseline(name)
			if paid {
				skipped = append(skipped, name)
			}
			return paid
		})
		if arm != "all" && len(skipped) > 0 {
			return fatal(fmt.Errorf("route: %s re-runs a paid model once per row; pass --allow-paid to spend, or --recorded to score stored results", arm))
		}
		if len(skipped) > 0 {
			fmt.Fprintf(os.Stderr, "skipping paid arms without --allow-paid: %s\n", strings.Join(skipped, ", "))
		}
		arms = free
	}
	if n := a.integer("limit", 0); n > 0 && n < len(examples) {
		examples = examples[len(examples)-n:]
	}
	baselines := routing.BaselineArms(history, float64(a.integer("arrival-seconds", routing.DefaultArrivalSeconds)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	defer printUsage(os.Stderr, router.Usage)
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
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "interrupted: reports cover only the rows scored before Ctrl-C")
		return 130
	}
	return 0
}

// printUsage reports provider calls and provider-counted input tokens. It
// prints nothing when no request left the process.
func printUsage(w io.Writer, usage func() map[string]routing.ProviderUsage) {
	all := usage()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		u := all[name]
		fmt.Fprintf(w, "provider calls: %s %d (input_tokens %d, reported on %d of them)\n", name, u.Calls, u.InputTokens, u.Counted)
	}
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
	fmt.Fprintf(w, "%-20s %-8s %6s %6s %6s %6s %6s %6s %6s %8s %8s", "arm", "source", "rows", "scored", "fb", "f@1", "f@3", "ch@1", "ch@3", "ans@1", "ms")
	for _, r := range reasons {
		fmt.Fprintf(w, " %s", r)
	}
	fmt.Fprintln(w)
	for _, rep := range reports {
		fmt.Fprintf(w, "%-20s %-8s %6d %6d %6d %6.3f %6.3f %6.3f %6.3f %8.3f %8.0f", rep.Arm, rep.Source, rep.Decisions, rep.Scored, rep.Fallbacks, rep.Fidelity1, rep.Fidelity3, rep.Chance1, rep.Chance3, rep.AnsweredFidelity1, rep.MeanLatencyMS)
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
				fmt.Fprintf(w, "  %-20s scored %d, fallbacks %d, hit@1 %.4g, hit@3 %.4g\n", rep.Arm, c.Scored, c.Fallbacks, float64(c.Hit1)+c.ExpectedHit1, float64(c.Hit3)+c.ExpectedHit3)
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
