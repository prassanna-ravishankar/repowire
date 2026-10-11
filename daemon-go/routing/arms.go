package routing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net/http"
	"sort"
	"time"
)

// Arm ranks candidates for one ask. Arms only read Input; they cannot spawn,
// kill, or deliver, and whatever they pick still passes CheckAccess.
type Arm interface {
	Name() string
	Rank(ctx context.Context, in Input) (RouterResult, error)
}

// Fallback reasons. Any arm failure maps to one of these and the ask falls
// back to explicit addressing.
const (
	FallbackTimeout      = "timeout"
	FallbackRateLimited  = "rate_limited"
	FallbackError        = "error"
	FallbackUnparseable  = "unparseable"
	FallbackNoCandidates = "no_candidates"
	FallbackUnconfigured = "unconfigured"
	FallbackUnknownArm   = "unknown_arm"
	FallbackPanic        = "panic"
	// FallbackBreakerOpen: the arm's provider failed repeatedly and is being
	// skipped until a probe succeeds.
	FallbackBreakerOpen = "breaker_open"
	// FallbackSaturated: a shadow arm was planned but the shadow pool was
	// full, so it did not run. Never applies to a live decision.
	FallbackSaturated = "saturated"
	// FallbackBilling (402) and FallbackUnauthorized (401/403): the provider
	// refused the account, as distinct from failing on the request.
	FallbackBilling      = "billing"
	FallbackUnauthorized = "unauthorized"
)

// statusReason classifies a provider's HTTP error status.
func statusReason(code int) string {
	switch code {
	case http.StatusTooManyRequests:
		return FallbackRateLimited
	case http.StatusPaymentRequired:
		return FallbackBilling
	case http.StatusUnauthorized, http.StatusForbidden:
		return FallbackUnauthorized
	}
	return FallbackError
}

// FallbackError classifies an arm failure.
type FallbackErr struct {
	Reason string
	Err    error
}

func (e *FallbackErr) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *FallbackErr) Unwrap() error { return e.Err }

func fail(reason string, format string, args ...any) error {
	return &FallbackErr{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// Run executes arm under timeout and returns a result that is always safe to
// record: on any failure Fallback is set with a reason and Scores is empty.
// A successful result is ranked best-first and only names eligible peers.
func Run(ctx context.Context, arm Arm, in Input, timeout time.Duration) RouterResult {
	start := time.Now()
	if len(in.Candidates) == 0 {
		return withIdentity(arm, fallback(arm.Name(), start, fail(FallbackNoCandidates, "no eligible candidates")))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := rankSafely(ctx, arm, in)
	if err == nil {
		err = validate(res, in)
	}
	if err != nil {
		if ctx.Err() != nil && generic(err) {
			err = &FallbackErr{Reason: FallbackTimeout, Err: err}
		}
		return withIdentity(arm, fallback(arm.Name(), start, err))
	}
	res.Arm = arm.Name()
	res.LatencyMS = time.Since(start).Milliseconds()
	sortScores(res.Scores, in)
	return res
}

// sortScores ranks best-first, breaking ties by candidate key order (c1, c2,
// ... in Input order) so equal scores pick the same peer live and in replay.
func sortScores(scores []Score, in Input) {
	pos := make(map[string]int, len(in.Candidates))
	for i, c := range in.Candidates {
		pos[c.PeerID] = i
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return pos[scores[i].PeerID] < pos[scores[j].PeerID]
	})
}

// identified arms report their configured model and template.
type identified interface {
	Identity() (model, template string)
}

// withIdentity stamps a failure with the arm's configured model and template,
// so failures group with the successes of the same configuration.
func withIdentity(arm Arm, res RouterResult) RouterResult {
	if id, ok := arm.(identified); ok {
		model, template := id.Identity()
		if res.Model == "" {
			res.Model = model
		}
		if res.TemplateSHA256 == "" {
			res.TemplateSHA256 = template
		}
	}
	return res
}

// rankSafely is the arm execution boundary: a panicking arm or SDK becomes a
// recorded panic fallback instead of taking the daemon down.
func rankSafely(ctx context.Context, arm Arm, in Input) (res RouterResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = RouterResult{}, fail(FallbackPanic, "arm panicked: %v", p)
		}
	}()
	return arm.Rank(ctx, in)
}

// fallbackResult is a fallback that never reached the arm.
func fallbackResult(arm, reason, msg string) RouterResult {
	return RouterResult{Arm: arm, Scores: []Score{}, Fallback: true, FallbackReason: reason, Error: msg}
}

func fallback(arm string, start time.Time, err error) RouterResult {
	reason := FallbackError
	var fe *FallbackErr
	if errors.As(err, &fe) {
		reason = fe.Reason
	}
	return RouterResult{
		Arm: arm, LatencyMS: time.Since(start).Milliseconds(), Scores: []Score{},
		Fallback: true, FallbackReason: reason, Error: clip(err.Error(), 500),
	}
}

// generic reports whether err carries no specific reason, so a deadline that
// expired during the call is the better explanation. Specific provider
// answers (rate limit, billing, unauthorized) and panics keep their reason.
func generic(err error) bool {
	var fe *FallbackErr
	return !errors.As(err, &fe) || fe.Reason == FallbackError || fe.Reason == FallbackUnparseable
}

// validate rejects any answer naming a peer outside the eligible set, or none.
func validate(res RouterResult, in Input) error {
	if len(res.Scores) == 0 {
		return fail(FallbackUnparseable, "arm returned no scores")
	}
	eligible := map[string]bool{}
	for _, c := range in.Candidates {
		eligible[c.PeerID] = true
	}
	for _, s := range res.Scores {
		if !eligible[s.PeerID] {
			return fail(FallbackUnparseable, "arm named ineligible peer %q", s.PeerID)
		}
	}
	return nil
}

// Random ranks candidates uniformly at random, seeded from the trace id so a
// replay reproduces the live draw.
type Random struct{}

func (Random) Name() string { return "random" }

func (Random) Rank(_ context.Context, in Input) (RouterResult, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(in.TraceID))
	seed := int64(h.Sum64() >> 1)
	order := rand.New(rand.NewSource(seed)).Perm(len(in.Candidates))
	scores := make([]Score, len(order))
	for rank, i := range order {
		scores[rank] = Score{PeerID: in.Candidates[i].PeerID, Score: rankScore(rank, len(order))}
	}
	return RouterResult{Scores: scores, Seed: &seed}, nil
}

// rankScore turns a rank into a score in (0,1] for arms that only order.
func rankScore(rank, n int) float64 { return float64(n-rank) / float64(n) }
