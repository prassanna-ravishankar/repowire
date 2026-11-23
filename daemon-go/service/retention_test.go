package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/state"
)

type fakeRetentionStore struct {
	mu      sync.Mutex
	sweeps  []bool // dry flag per call
	policy  state.RetentionPolicy
	counts  map[string]int
	block   chan struct{} // when set, Sweep waits on it
	vacuums int
}

func (f *fakeRetentionStore) Sweep(_ context.Context, p state.RetentionPolicy, _ time.Time, dry bool) (state.SweepReport, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, dry)
	f.policy = p
	counts := map[string]int{}
	for k, v := range f.counts {
		counts[k] = v
	}
	return state.SweepReport{Counts: counts}, nil
}

func (f *fakeRetentionStore) Vacuum(context.Context) error {
	f.mu.Lock()
	f.vacuums++
	f.mu.Unlock()
	return nil
}

func (f *fakeRetentionStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sweeps)
}

// syncRetention runs lazy sweeps inline and on a controllable clock.
func syncRetention(store RetentionStore, cfg config.RetentionConfig, dir string, now *time.Time) *Retention {
	r := NewRetention(store, cfg, dir).WithSpawn(func(f func()) { f() })
	r.now = func() time.Time { return *now }
	return r
}

func TestMaybeSweepRunsAtMostHourly(t *testing.T) {
	store := &fakeRetentionStore{}
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := syncRetention(store, config.RetentionConfig{ContentDays: 7, RecordsDays: 180}, "", &now)

	r.MaybeSweep(context.Background())
	r.MaybeSweep(context.Background())
	if got := store.calls(); got != 1 {
		t.Fatalf("sweeps within the hour = %d, want 1", got)
	}
	now = now.Add(59 * time.Minute)
	r.MaybeSweep(context.Background())
	if got := store.calls(); got != 1 {
		t.Fatalf("sweeps at 59m = %d, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	r.MaybeSweep(context.Background())
	if got := store.calls(); got != 2 {
		t.Fatalf("sweeps after an hour = %d, want 2", got)
	}
	if store.policy != (state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 180 * 24 * time.Hour}) {
		t.Fatalf("policy = %+v", store.policy)
	}
}

func TestMaybeSweepIsSingleFlightAndOffThePath(t *testing.T) {
	store := &fakeRetentionStore{block: make(chan struct{})}
	r := NewRetention(store, config.RetentionConfig{ContentDays: 7}, "")
	var wg sync.WaitGroup
	r.WithSpawn(func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	})

	done := make(chan struct{})
	go func() {
		r.MaybeSweep(context.Background()) // must return while the sweep blocks
		r.MaybeSweep(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MaybeSweep waited for the sweep")
	}
	if _, err := r.Sweep(context.Background(), false, false); !errors.Is(err, ErrSweepRunning) {
		t.Fatalf("sweep during a sweep: %v", err)
	}
	close(store.block)
	wg.Wait()
	if got := store.calls(); got != 1 {
		t.Fatalf("sweeps = %d, want 1", got)
	}
}

func TestSweepJournalsOnlyChangesAndDryRunKeepsSchedule(t *testing.T) {
	store := &fakeRetentionStore{}
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := syncRetention(store, config.RetentionConfig{ContentDays: 7}, "", &now)
	var events []map[string]any
	r.WithJournal(func(_ context.Context, typ string, payload map[string]any) string {
		if typ != "retention_swept" {
			t.Fatalf("event type %q", typ)
		}
		events = append(events, payload)
		return ""
	})

	if _, err := r.Sweep(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatal("a sweep that changed nothing was journaled")
	}
	store.counts = map[string]int{"events.strip": 3}
	if _, err := r.Sweep(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || r.Status().LastSweep.Total() != 0 {
		t.Fatal("a dry run was journaled or recorded as the last sweep")
	}
	now = now.Add(2 * time.Hour)
	if _, err := r.Sweep(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || store.vacuums != 1 {
		t.Fatalf("events=%v vacuums=%d", events, store.vacuums)
	}
	if st := r.Status(); st.LastSweepAt == nil || !st.LastSweepAt.Equal(now) || st.LastSweep.Counts["events.strip"] != 3 {
		t.Fatalf("status = %+v", st)
	}
}

func TestSweepRemovesExpiredAttachmentsOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	write := func(name string, age time.Duration) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("old.png", 8*24*time.Hour)
	write("new.png", 6*24*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := syncRetention(&fakeRetentionStore{}, config.RetentionConfig{ContentDays: 7, RecordsDays: 180}, dir, &now)
	dry, _ := r.Sweep(context.Background(), true, false)
	if dry.Counts["attachments.delete"] != 1 {
		t.Fatalf("dry counts = %v", dry.Counts)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.png")); err != nil {
		t.Fatal("dry run removed a file")
	}
	report, _ := r.Sweep(context.Background(), false, false)
	if report.Counts["attachments.delete"] != 1 {
		t.Fatalf("counts = %v", report.Counts)
	}
	for name, want := range map[string]bool{"old.png": false, "new.png": true, "sub": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}

	forever := syncRetention(&fakeRetentionStore{}, config.RetentionConfig{}, dir, &now)
	if !forever.FileCutoff(now).IsZero() {
		t.Fatal("keep-forever policy has a file cutoff")
	}
	short := syncRetention(&fakeRetentionStore{}, config.RetentionConfig{ContentDays: 7, RecordsDays: 2}, dir, &now)
	if got := short.FileCutoff(now); !got.Equal(now.Add(-2 * 24 * time.Hour)) {
		t.Fatalf("records shorter than content: cutoff %v", got)
	}
}
