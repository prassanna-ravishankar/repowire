package service

// Wall-clock waiting for the time-driven loops (Scheduler, JobRunner).
//
// Go timers on darwin run on mach_absolute_time, which stops while the Mac
// sleeps, so a timer measures awake time: a 30-minute timer armed before the
// lid closes fires after 30 minutes of accumulated awake time, hours late by
// the wall clock (repowire-2ph). waitUntil therefore never arms a timer longer
// than one chunk, and re-reads the wall clock after each. A chunk does no other
// work: no store query, no I/O.
//
// Bound: a deadline that passes while the machine sleeps is noticed within one
// chunk (30s) of awake time after it wakes, because no armed timer has more
// than a chunk left to run. 30s sits under the ~45s DarkWakes macOS grants in
// clamshell sleep, so an overdue fire normally lands in the first DarkWake
// after its deadline instead of accumulating across several. And waitUntil
// never returns before the wall-clock deadline, so a timer that runs a moment
// fast cannot fire work early (repowire-5je).

import (
	"context"
	"time"
)

const wallClockChunk = 30 * time.Second

// wallClock is the loops' clock: now reads the wall clock and chunk bounds a
// single timer. Tests shrink chunk and fake now.
type wallClock struct {
	now   func() time.Time
	chunk time.Duration
}

func systemWallClock() wallClock { return wallClock{now: time.Now, chunk: wallClockChunk} }

// waitUntil blocks until the wall clock reaches deadline, returning
// (woken, stopped): woken when wake fired first, stopped when stop or ctx did.
// (false, false) means the deadline has been reached.
func (c wallClock) waitUntil(ctx context.Context, deadline time.Time, wake, stop <-chan struct{}) (woken, stopped bool) {
	// Round(0) drops the monotonic reading so Sub compares wall clocks.
	deadline = deadline.Round(0)
	for {
		remaining := deadline.Sub(c.now().Round(0))
		if remaining <= 0 {
			return false, false
		}
		t := time.NewTimer(min(remaining, c.chunk))
		select {
		case <-t.C:
		case <-wake:
			t.Stop()
			return true, false
		case <-stop:
			t.Stop()
			return false, true
		case <-ctx.Done():
			t.Stop()
			return false, true
		}
	}
}
