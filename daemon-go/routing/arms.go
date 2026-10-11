package routing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
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
)

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
		return fallback(arm.Name(), start, fail(FallbackNoCandidates, "no eligible candidates"))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := arm.Rank(ctx, in)
	if err == nil {
		err = validate(res, in)
	}
	if err != nil {
		if ctx.Err() != nil && !isFallback(err, FallbackRateLimited) {
			err = &FallbackErr{Reason: FallbackTimeout, Err: err}
		}
		return fallback(arm.Name(), start, err)
	}
	res.Arm = arm.Name()
	res.LatencyMS = time.Since(start).Milliseconds()
	sort.SliceStable(res.Scores, func(i, j int) bool { return res.Scores[i].Score > res.Scores[j].Score })
	return res
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

func isFallback(err error, reason string) bool {
	var fe *FallbackErr
	return errors.As(err, &fe) && fe.Reason == reason
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
