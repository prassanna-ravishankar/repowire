package routing

import (
	"slices"
	"sync"
	"time"
)

// Breaker thresholds. The breaker is checked on each request, never on a
// timer: it opens after breakerTrip consecutive provider failures, rejects
// calls for breakerCooldown, then lets the next request through as the single
// probe. A successful probe closes it; a failed one reopens it.
const (
	breakerTrip     = 3
	breakerCooldown = 30 * time.Second
)

// providerFailure reports whether a fallback reason blames the provider.
// Our own problems (no candidates, unknown arm, missing key, a 4xx
// request_error from a bad body or template) do not trip it: three template
// bugs must not suppress every view of a healthy provider. A key the provider
// rejects (unauthorized) or an unfunded account (billing) does, since every
// further call would fail the same way.
func providerFailure(reason string) bool {
	switch reason {
	case FallbackTimeout, FallbackRateLimited, FallbackError, FallbackServerError, FallbackUnparseable, FallbackPanic, FallbackBilling, FallbackUnauthorized:
		return true
	}
	return false
}

type breaker struct {
	mu       sync.Mutex
	failures int
	// models are the models behind the current run of failures, so a
	// breaker_open row can say whether its own model was among them.
	models   []string
	openedAt time.Time
	probing  bool
	now      func() time.Time
}

// allow reports whether a call may proceed, and whether it is the probe.
func (b *breaker) allow() (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < breakerTrip {
		return true, false
	}
	if b.probing || b.now().Sub(b.openedAt) < breakerCooldown {
		return false, false
	}
	b.probing = true
	return true, true
}

// record feeds back one call's result.
func (b *breaker) record(res RouterResult, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	if res.Fallback && providerFailure(res.FallbackReason) {
		b.failures++
		if !slices.Contains(b.models, res.Model) {
			b.models = append(b.models, res.Model)
		}
		if b.failures >= breakerTrip {
			b.openedAt = b.now()
		}
		return
	}
	if !res.Fallback {
		b.failures, b.models = 0, nil
	}
}

// gatedBy lists the models whose failures hold the breaker open, sorted.
func (b *breaker) gatedBy() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	models := slices.Clone(b.models)
	slices.Sort(models)
	return models
}
