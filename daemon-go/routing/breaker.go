package routing

import (
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
// Input problems (no candidates, unknown arm, missing key) do not trip it; a
// key the provider rejects (unauthorized) or an unfunded account (billing)
// does, since every further call would fail the same way.
func providerFailure(reason string) bool {
	switch reason {
	case FallbackTimeout, FallbackRateLimited, FallbackError, FallbackUnparseable, FallbackPanic, FallbackBilling, FallbackUnauthorized:
		return true
	}
	return false
}

type breaker struct {
	mu       sync.Mutex
	failures int
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
		if b.failures >= breakerTrip {
			b.openedAt = b.now()
		}
		return
	}
	if !res.Fallback {
		b.failures = 0
	}
}
