package service

import (
	"context"
	"errors"
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

// ErrSweepRunning refuses a sweep requested while another is in flight.
var ErrSweepRunning = errors.New("a retention sweep is already running")

// RetentionStore is the persistence seam; *state.Store satisfies it.
type RetentionStore interface {
	Sweep(ctx context.Context, p state.RetentionPolicy, now time.Time, dry bool) (state.SweepReport, error)
	Vacuum(ctx context.Context) error
}

// RetentionStatus is what GET /retention reports.
type RetentionStatus struct {
	ContentDays float64            `json:"content_days"`
	RecordsDays float64            `json:"records_days"`
	LastSweepAt *time.Time         `json:"last_sweep_at"`
	LastSweep   *state.SweepReport `json:"last_sweep"`
}

type Retention struct {
	store   RetentionStore
	cfg     config.RetentionConfig
	files   string // attachments directory; empty skips files
	journal func(ctx context.Context, typ string, payload map[string]any) string
	spawn   func(func())
	now     func() time.Time

	mu      sync.Mutex
	running bool
	due     time.Time // no lazy sweep before this
	lastAt  *time.Time
	last    *state.SweepReport
}

// NewRetention returns the sweeper for cfg. files is the attachments
// directory.
func NewRetention(store RetentionStore, cfg config.RetentionConfig, files string) *Retention {
	return &Retention{store: store, cfg: cfg, files: files, now: time.Now, spawn: func(f func()) { go f() }}
}

// WithJournal records a retention_swept event for every sweep that changed
// something.
func (r *Retention) WithJournal(fn func(ctx context.Context, typ string, payload map[string]any) string) *Retention {
	r.journal = fn
	return r
}

// WithSpawn runs lazy sweeps on a tracked goroutine, so shutdown waits for
// one in flight.
func (r *Retention) WithSpawn(fn func(func())) *Retention {
	r.spawn = fn
	return r
}

// Policy is the configured policy as durations.
func (r *Retention) Policy() state.RetentionPolicy {
	return state.RetentionPolicy{Content: days(r.cfg.ContentDays), Records: days(r.cfg.RecordsDays)}
}

func days(n float64) time.Duration { return time.Duration(n * float64(24*time.Hour)) }

// MaybeSweep starts a sweep off the caller's path when one is due. Cheap
// otherwise: lazy repair calls it on every pass.
func (r *Retention) MaybeSweep(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.running || r.now().Before(r.due) {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	r.spawn(func() {
		if _, err := r.run(ctx, false); err != nil {
			log.Printf("retention: %v", err)
		}
	})
}

// Sweep runs one sweep now (dry only counts). vacuum then rebuilds the
// database file. Refused while another sweep runs.
func (r *Retention) Sweep(ctx context.Context, dry, vacuum bool) (state.SweepReport, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return state.SweepReport{}, ErrSweepRunning
	}
	r.running = true
	r.mu.Unlock()
	report, err := r.run(ctx, dry)
	if err == nil && vacuum && !dry {
		err = r.store.Vacuum(ctx)
	}
	return report, err
}

// run sweeps and releases the running flag. Callers set it.
func (r *Retention) run(ctx context.Context, dry bool) (state.SweepReport, error) {
	now := r.now()
	report, err := r.store.Sweep(ctx, r.Policy(), now, dry)
	if report.Counts == nil {
		report.Counts = map[string]int{}
	}
	if n := r.sweepFiles(now, dry); n > 0 {
		report.Counts["attachments.delete"] = n
	}
	r.mu.Lock()
	r.running = false
	if !dry {
		r.due = now.Add(retentionInterval)
		at := now
		r.lastAt, r.last = &at, &report
	}
	r.mu.Unlock()
	if !dry && report.Total() > 0 {
		log.Printf("retention: swept %d rows %v", report.Total(), report.Counts)
		if r.journal != nil {
			r.journal(ctx, "retention_swept", map[string]any{"counts": report.Counts, "errors": report.Errors})
		}
	}
	return report, err
}

// FileCutoff is the oldest modification time an attachment may have; zero
// when content is kept forever.
func (r *Retention) FileCutoff(now time.Time) time.Time {
	if r == nil {
		return time.Time{}
	}
	content, _ := r.Policy().Cutoffs(now)
	return content
}

func (r *Retention) sweepFiles(now time.Time, dry bool) int {
	if r.files == "" {
		return 0
	}
	return SweepFiles(r.files, r.FileCutoff(now), dry)
}

// SweepFiles removes (or, dry, counts) regular files in dir modified before
// cutoff. A zero cutoff keeps everything. Best-effort.
func SweepFiles(dir string, cutoff time.Time, dry bool) int {
	if cutoff.IsZero() {
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
		if dry || os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// Status reports the policy and the last sweep.
func (r *Retention) Status() RetentionStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RetentionStatus{ContentDays: r.cfg.ContentDays, RecordsDays: r.cfg.RecordsDays, LastSweepAt: r.lastAt, LastSweep: r.last}
}
