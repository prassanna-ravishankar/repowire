package service

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/state"
)

// Retention runs the one retention policy (rfcs/retention.md) over the store
// and the attachments directory. It never runs on a timer: lazy repair asks
// for a sweep, and one runs when an hour has passed since the last.

const retentionInterval = time.Hour

// RetentionStore is the persistence seam; *state.Store satisfies it.
type RetentionStore interface {
	Sweep(ctx context.Context, p state.RetentionPolicy, now time.Time) (map[string]int, error)
}

type Retention struct {
	store   RetentionStore
	policy  state.RetentionPolicy
	files   string // attachments directory; empty skips files
	journal func(ctx context.Context, typ string, payload map[string]any) string
	spawn   func(func())
	now     func() time.Time

	mu      sync.Mutex
	running bool
	due     time.Time // no sweep before this
}

// NewRetention returns the sweeper for cfg. files is the attachments
// directory.
func NewRetention(store RetentionStore, cfg config.RetentionConfig, files string) *Retention {
	return &Retention{
		store:  store,
		policy: state.RetentionPolicy{Content: days(cfg.ContentDays), Records: days(cfg.RecordsDays)},
		files:  files,
		now:    time.Now,
		spawn:  func(f func()) { go f() },
	}
}

func days(n float64) time.Duration { return time.Duration(n * float64(24*time.Hour)) }

// WithJournal records a retention_swept event for every sweep that changed
// something.
func (r *Retention) WithJournal(fn func(ctx context.Context, typ string, payload map[string]any) string) *Retention {
	r.journal = fn
	return r
}

// WithSpawn runs sweeps on a tracked goroutine, so shutdown waits for one in
// flight.
func (r *Retention) WithSpawn(fn func(func())) *Retention {
	r.spawn = fn
	return r
}

// MaybeSweep starts a sweep off the caller's path when one is due and none
// is running. Cheap otherwise: lazy repair calls it on every pass.
func (r *Retention) MaybeSweep(ctx context.Context) {
	r.mu.Lock()
	now := r.now()
	if r.running || now.Before(r.due) {
		r.mu.Unlock()
		return
	}
	r.running, r.due = true, now.Add(retentionInterval)
	r.mu.Unlock()

	ctx = context.WithoutCancel(ctx)
	r.spawn(func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()
		counts, err := r.store.Sweep(ctx, r.policy, now)
		if err != nil {
			log.Printf("retention: %v", err)
		}
		if n := sweepFiles(r.files, r.policy.ContentCutoff(now)); n > 0 {
			counts["attachments.delete"] = n
		}
		if len(counts) == 0 {
			return
		}
		log.Printf("retention: swept %v", counts)
		if r.journal != nil {
			r.journal(ctx, "retention_swept", map[string]any{"counts": counts})
		}
	})
}

// sweepFiles removes regular files in dir modified before cutoff. A zero
// cutoff or an empty dir keeps everything. Best-effort.
func sweepFiles(dir string, cutoff time.Time) int {
	if dir == "" || cutoff.IsZero() {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}
