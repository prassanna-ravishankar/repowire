package routing

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// Example is one recorded decision with what followed it in its trace.
type Example struct {
	TraceID  string
	Decision Decision
	// Shadow holds the recorded route_shadow result for each arm that ran.
	Shadow  map[string]RouterResult
	Outcome Outcome
}

// Delivery outcomes of a recorded ask, derived from its trace.
const (
	DeliveryDelivered    = "delivered"
	DeliveryQuiesced     = "quiesced"
	DeliveryNoConnection = "no_connection"
	DeliverySendFailed   = "send_failed"
	DeliveryNotAttempted = "not_attempted" // resolution failed before registration
	// DeliverySent: written to the peer's socket, receipt not confirmed.
	DeliverySent = "sent_unverified"
	// DeliveryUnknown: neither success nor failure is evidenced (a crash after
	// the decision, a cancelled request, or delivery still in progress).
	DeliveryUnknown = "unknown"
)

// deliveryOutcome classifies an ask from positive evidence only. Any failure
// stage wins; otherwise an accepted receipt, or the recipient closing the ask,
// is needed to call it delivered. Absence of evidence stays unknown.
func deliveryOutcome(stages []state.TraceRow) string {
	failed, accepted, sent := "", false, false
	for _, st := range stages {
		switch st.Stage {
		case "route_failed":
			// With a reason: resolution failed before registration (or the
			// recipient was quiesced). Without one: delivery itself failed.
			switch reason, _ := st.Detail["reason"].(string); reason {
			case "":
				failed = DeliverySendFailed
			case "quiesced":
				failed = DeliveryQuiesced
			default:
				failed = DeliveryNotAttempted
			}
		case "no_connection":
			failed = DeliveryNoConnection
		case "injection_failed":
			if failed == "" {
				failed = DeliverySendFailed
			}
		case "thread_input_accepted", "pane_injected", "acked":
			accepted = true
		case "websocket_sent":
			sent = true
		case "closed":
			if reason, _ := st.Detail["close_reason"].(string); reason == "send_failed" {
				if failed == "" {
					failed = DeliverySendFailed
				}
			} else {
				accepted = true
			}
		}
	}
	switch {
	case failed != "":
		return failed
	case accepted:
		return DeliveryDelivered
	case sent:
		return DeliverySent
	}
	return DeliveryUnknown
}

// Outcome is how the ask ended, from its trace's later stages.
type Outcome struct {
	Delivery       string `json:"delivery"`
	CloseReason    string `json:"close_reason,omitempty"`
	RecipientTurns *int   `json:"recipient_turns,omitempty"`
	TimeToCloseMS  *int64 `json:"time_to_close_ms,omitempty"`
}

// Ledger is the slice of the state store replay reads.
type Ledger interface {
	TracesWithStage(ctx context.Context, stage string, since time.Time) ([]state.TraceRow, error)
	StagesFor(ctx context.Context, traceID string) ([]state.TraceRow, error)
}

// LoadExamples reads every recorded decision after since, joined with its
// shadow results and outcome.
func LoadExamples(ctx context.Context, ledger Ledger, since time.Time) ([]Example, error) {
	rows, err := ledger.TracesWithStage(ctx, Stage, since)
	if err != nil {
		return nil, err
	}
	out := make([]Example, 0, len(rows))
	for _, row := range rows {
		d, err := ParseDecision(row.Detail)
		if err != nil || d.Schema != SchemaV1 {
			continue
		}
		ex := Example{TraceID: row.TraceID, Decision: d, Shadow: map[string]RouterResult{}}
		stages, err := ledger.StagesFor(ctx, row.TraceID)
		if err != nil {
			return nil, err
		}
		for _, st := range stages {
			switch st.Stage {
			case ShadowStage:
				var res RouterResult
				if remap(st.Detail, &res) == nil {
					ex.Shadow[res.Arm] = res
				}
			case "closed":
				_ = remap(st.Detail, &ex.Outcome)
			}
		}
		// Arms the decision itself records as skipped (e.g. a saturated
		// shadow pool) have no row of their own.
		for _, skipped := range d.ShadowSkipped {
			if _, ok := ex.Shadow[skipped.Arm]; !ok {
				ex.Shadow[skipped.Arm] = skipped
			}
		}
		ex.Outcome.Delivery = deliveryOutcome(stages)
		out = append(out, ex)
	}
	return out, nil
}

// Skip reasons: why an example does not count toward fidelity.
const (
	SkipNotExplicit        = "not_explicit"        // the router chose, so no human label
	SkipNoDeclaredTarget   = "no_declared_target"  // nothing named, or it did not resolve
	SkipDeclaredIneligible = "declared_ineligible" // the label is not among the arm's choices
	SkipTextNotRecorded    = "text_not_recorded"   // a text-reading arm cannot be re-run
	SkipNotSampled         = "not_sampled"         // recorded mode: the arm was not sampled for this ask
	// "<prefix>:<reason>" marks an arm meant to run that did not:
	// planned_not_run:missing (no row: restart, or still in flight), and
	// saturated / breaker_open in either mode.
	SkipPlannedNotRun = "planned_not_run"
	SkipNotRun        = "not_run"
)

// Result is one example's replay.
type Result struct {
	TraceID        string   `json:"trace_id"`
	Surface        string   `json:"surface"`
	Declared       string   `json:"declared,omitempty"`
	Top            []string `json:"top,omitempty"`
	Hit1           bool     `json:"hit1"`
	Hit3           bool     `json:"hit3"`
	Candidates     int      `json:"candidates"`
	Fallback       bool     `json:"fallback,omitempty"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
	// FallbackStatus and FallbackProvider attribute a failure (our request
	// or their service) without its body.
	FallbackStatus   int    `json:"fallback_status,omitempty"`
	FallbackProvider string `json:"fallback_provider,omitempty"`
	// FallbackDetail is the recorded error, scrubbed again on export: with
	// credentials and the ask redacted, or, for asks recorded without their
	// text, only "<status> <reason>".
	FallbackDetail string  `json:"fallback_detail,omitempty"`
	LatencyMS      int64   `json:"latency_ms"`
	Skipped        string  `json:"skipped,omitempty"`
	Outcome        Outcome `json:"outcome"`
	// DeclaredIsRecentContact: the sender's pick was the candidate it
	// exchanged with most recently (from the unmasked snapshot).
	DeclaredIsRecentContact bool `json:"declared_is_recent_contact"`
	// Recency is the stratum: recent contact, other, or no exchange data.
	Recency string `json:"recency"`
	// Mask lists the candidate fields blanked before the arm ran.
	Mask []string `json:"mask,omitempty"`
}

// Report aggregates a replay. Fidelity counts a fallback as a miss; the
// answered variants exclude fallbacks.
type Report struct {
	Arm               string         `json:"arm"`
	Source            string         `json:"source"` // "rerun" or "recorded"
	Mask              []string       `json:"mask,omitempty"`
	Decisions         int            `json:"decisions"`
	Scored            int            `json:"scored"`
	Fallbacks         int            `json:"fallbacks"`
	Hit1              int            `json:"hit1"`
	Hit3              int            `json:"hit3"`
	Fidelity1         float64        `json:"fidelity_at_1"`
	Fidelity3         float64        `json:"fidelity_at_3"`
	AnsweredFidelity1 float64        `json:"answered_fidelity_at_1"`
	AnsweredFidelity3 float64        `json:"answered_fidelity_at_3"`
	MeanCandidates    float64        `json:"mean_candidates"`
	MeanLatencyMS     float64        `json:"mean_latency_ms"`
	Skipped           map[string]int `json:"skipped"`
	// FallbackDetails counts each distinct recorded error per fallback
	// reason. JSON only; the table shows reasons, not details.
	FallbackDetails map[string]map[string]int `json:"fallback_details,omitempty"`
	// History is the vote population of a history baseline, which can be
	// wider than the scored rows.
	History *History `json:"history,omitempty"`
	// Cohorts splits the scored asks by delivery outcome. Failed deliveries
	// stay in the headline numbers and are also reported here on their own.
	Cohorts map[string]*Cohort `json:"cohorts"`
	// Recency splits the scored asks by whether the sender picked its most
	// recent contact. The rows where it did not are the real test of an
	// attested arm.
	Recency map[string]*Cohort `json:"recency"`
}

// Recency strata.
const (
	StratumRecentContact = "declared_recent_contact"
	StratumOtherContact  = "declared_other"
	// StratumNoExchange: no candidate had a recorded exchange with the
	// sender (e.g. right after a daemon restart), so recency is unknown.
	StratumNoExchange = "no_exchange_data"
)

// Options select how Replay scores an arm.
type Options struct {
	// Recorded scores the arm's recorded shadow results and makes no calls.
	Recorded bool
	// Mask blanks these candidate fields (from ParseMask) before a re-run.
	Mask []string
	// Secrets are literals (configured API keys) redacted from exported
	// fallback details, on top of the credential shapes.
	Secrets []string
}

// Cohort is the scored subset with one delivery outcome.
type Cohort struct {
	Scored    int `json:"scored"`
	Fallbacks int `json:"fallbacks"`
	Hit1      int `json:"hit1"`
	Hit3      int `json:"hit3"`
}

// Replay scores an arm against the sender's declared target on every labelled
// example. With opts.Recorded it scores the arm's recorded shadow results and
// makes no calls; otherwise it re-runs runner on the recorded candidate set,
// masked by opts.Mask.
func Replay(ctx context.Context, examples []Example, armName string, runner func(context.Context, Input) RouterResult, opts Options) (Report, []Result) {
	recorded := opts.Recorded
	rep := Report{Arm: armName, Source: "rerun", Decisions: len(examples), Skipped: map[string]int{}, Cohorts: map[string]*Cohort{}, Recency: map[string]*Cohort{}}
	if recorded {
		rep.Source = "recorded"
	} else {
		rep.Mask = opts.Mask
	}
	var latency, candidates float64
	results := make([]Result, 0, len(examples))
	for _, ex := range examples {
		d := ex.Decision
		in := InputFrom(ex.TraceID, d)
		r := Result{TraceID: ex.TraceID, Surface: d.Surface, Candidates: len(in.Candidates), Outcome: ex.Outcome, Mask: rep.Mask}
		if d.Declared.PeerID != nil {
			r.Declared = *d.Declared.PeerID
			r.DeclaredIsRecentContact = DeclaredIsRecentContact(in, r.Declared)
		}
		r.Recency = recencyStratum(in, r.DeclaredIsRecentContact)
		r.Skipped = skipReason(d, in, armName, recorded, ex.Shadow)
		if r.Skipped != "" {
			rep.Skipped[r.Skipped]++
			results = append(results, r)
			continue
		}
		var res RouterResult
		if recorded {
			res = ex.Shadow[armName]
		} else {
			res = runner(ctx, Mask(in, opts.Mask))
		}
		if res.Fallback && (res.FallbackReason == FallbackSaturated || res.FallbackReason == FallbackBreakerOpen) {
			// The arm never ran: no evidence about its routing either way.
			prefix := SkipNotRun
			if recorded {
				prefix = SkipPlannedNotRun
			}
			r.Skipped = prefix + ":" + res.FallbackReason
			rep.Skipped[r.Skipped]++
			results = append(results, r)
			continue
		}
		r.Fallback, r.FallbackReason, r.LatencyMS = res.Fallback, res.FallbackReason, res.LatencyMS
		if res.Fallback {
			r.FallbackStatus, r.FallbackProvider = statusOf(res), res.Provider // before the text is scrubbed
			res = Scrub(res, d.Request.TextRecorded, append(slices.Clone(opts.Secrets), d.Request.Text)...)
			r.FallbackDetail = res.Error
			if r.FallbackProvider == "" {
				r.FallbackProvider = providerOf(armName)
			}
			if rep.FallbackDetails == nil {
				rep.FallbackDetails = map[string]map[string]int{}
			}
			if rep.FallbackDetails[res.FallbackReason] == nil {
				rep.FallbackDetails[res.FallbackReason] = map[string]int{}
			}
			rep.FallbackDetails[res.FallbackReason][res.Error]++
		}
		scores := res.Scores
		if res.Fallback {
			scores = nil // a fallback is a miss whatever it carries
		}
		for i, s := range scores {
			if i == 3 {
				break
			}
			r.Top = append(r.Top, s.PeerID)
			if s.PeerID == r.Declared {
				r.Hit3 = true
				r.Hit1 = r.Hit1 || i == 0
			}
		}
		rep.Scored++
		latency += float64(res.LatencyMS)
		candidates += float64(r.Candidates)
		if r.Fallback {
			rep.Fallbacks++
		}
		if r.Hit1 {
			rep.Hit1++
		}
		if r.Hit3 {
			rep.Hit3++
		}
		tally(rep.Cohorts, ex.Outcome.Delivery, r)
		tally(rep.Recency, r.Recency, r)
		results = append(results, r)
	}
	if rep.Scored > 0 {
		rep.Fidelity1 = float64(rep.Hit1) / float64(rep.Scored)
		rep.Fidelity3 = float64(rep.Hit3) / float64(rep.Scored)
		rep.MeanLatencyMS = latency / float64(rep.Scored)
		rep.MeanCandidates = candidates / float64(rep.Scored)
	}
	if answered := rep.Scored - rep.Fallbacks; answered > 0 {
		rep.AnsweredFidelity1 = float64(rep.Hit1) / float64(answered)
		rep.AnsweredFidelity3 = float64(rep.Hit3) / float64(answered)
	}
	return rep, results
}

func recencyStratum(in Input, recent bool) string {
	switch {
	case recent:
		return StratumRecentContact
	case slices.ContainsFunc(in.Candidates, func(c Candidate) bool { _, ok := exchangedAt(c); return ok }):
		return StratumOtherContact
	}
	return StratumNoExchange
}

// providerOf names a model arm's provider from its name, for rows recorded
// before results carried it.
func providerOf(arm string) string {
	switch {
	case strings.HasPrefix(arm, "jev-"):
		return Jev{}.Provider()
	case strings.HasPrefix(arm, "frontier-"):
		return Frontier{}.Provider()
	}
	return ""
}

// tally adds a scored result to the named cohort.
func tally(cohorts map[string]*Cohort, name string, r Result) {
	c := cohorts[name]
	if c == nil {
		c = &Cohort{}
		cohorts[name] = c
	}
	c.Scored++
	if r.Fallback {
		c.Fallbacks++
	}
	if r.Hit1 {
		c.Hit1++
	}
	if r.Hit3 {
		c.Hit3++
	}
}

func skipReason(d Decision, in Input, arm string, recorded bool, shadow map[string]RouterResult) string {
	if d.Arm != ArmExplicit {
		return SkipNotExplicit
	}
	if d.Declared.Resolution != ResolutionResolved || d.Declared.PeerID == nil || d.Chosen == nil {
		return SkipNoDeclaredTarget
	}
	eligible := false
	for _, c := range in.Candidates {
		eligible = eligible || c.PeerID == *d.Declared.PeerID
	}
	if !eligible {
		return SkipDeclaredIneligible
	}
	if recorded {
		planned := false
		for _, name := range d.ShadowPlanned {
			planned = planned || name == arm
		}
		if !planned {
			return SkipNotSampled
		}
		if _, ok := shadow[arm]; !ok {
			return SkipPlannedNotRun + ":missing"
		}
		return ""
	}
	if !d.Request.TextRecorded && !IsBaseline(arm) {
		return SkipTextNotRecorded
	}
	return ""
}

// remap decodes a trace detail map into a typed value.
func remap(detail map[string]any, out any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
