package routing

import (
	"context"
	"encoding/json"
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

// Outcome is how the ask ended, from its trace's closed stage.
type Outcome struct {
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
	SkipNotSampled         = "not_sampled"         // recorded mode: the arm did not run here
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
	LatencyMS      int64    `json:"latency_ms"`
	Skipped        string   `json:"skipped,omitempty"`
	Outcome        Outcome  `json:"outcome"`
}

// Report aggregates a replay. Fidelity counts a fallback as a miss; the
// answered variants exclude fallbacks.
type Report struct {
	Arm               string         `json:"arm"`
	Source            string         `json:"source"` // "rerun" or "recorded"
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
}

// Replay scores an arm against the sender's declared target on every labelled
// example. With recorded set it scores the arm's recorded shadow results and
// makes no calls; otherwise it re-runs runner on the recorded candidate set.
func Replay(ctx context.Context, examples []Example, armName string, runner func(context.Context, Input) RouterResult, recorded bool) (Report, []Result) {
	rep := Report{Arm: armName, Source: "rerun", Decisions: len(examples), Skipped: map[string]int{}}
	if recorded {
		rep.Source = "recorded"
	}
	var latency, candidates float64
	results := make([]Result, 0, len(examples))
	for _, ex := range examples {
		d := ex.Decision
		in := InputFrom(ex.TraceID, d)
		r := Result{TraceID: ex.TraceID, Surface: d.Surface, Candidates: len(in.Candidates), Outcome: ex.Outcome}
		if d.Declared.PeerID != nil {
			r.Declared = *d.Declared.PeerID
		}
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
			res = runner(ctx, in)
		}
		r.Fallback, r.FallbackReason, r.LatencyMS = res.Fallback, res.FallbackReason, res.LatencyMS
		for i, s := range res.Scores {
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
		if _, ok := shadow[arm]; !ok {
			return SkipNotSampled
		}
		return ""
	}
	if !d.Request.TextRecorded && arm != "random" {
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
