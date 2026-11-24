package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/state"
)

type fakeRetentionStore struct {
	mu     sync.Mutex
	calls  int
	policy state.RetentionPolicy
	counts map[string]int
	block  chan struct{} // when set, Sweep waits on it
}

func (f *fakeRetentionStore) Sweep(_ context.Context, p state.RetentionPolicy, _ time.Time) (map[string]int, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.policy = p
	counts := map[string]int{}
	for k, v := range f.counts {
		counts[k] = v
	}
	return counts, nil
}

func (f *fakeRetentionStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// syncRetention runs sweeps inline and on a controllable clock.
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
	if got := store.callCount(); got != 1 {
		t.Fatalf("sweeps within the hour = %d, want 1", got)
	}
	now = now.Add(59 * time.Minute)
	r.MaybeSweep(context.Background())
	if got := store.callCount(); got != 1 {
		t.Fatalf("sweeps at 59m = %d, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	r.MaybeSweep(context.Background())
	if got := store.callCount(); got != 2 {
		t.Fatalf("sweeps after an hour = %d, want 2", got)
	}
	if store.policy != (state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 180 * 24 * time.Hour}) {
		t.Fatalf("policy = %+v", store.policy)
	}
}

func TestMaybeSweepIsSingleFlightAndOffThePath(t *testing.T) {
	store := &fakeRetentionStore{block: make(chan struct{})}
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := NewRetention(store, config.RetentionConfig{ContentDays: 7}, "")
	var mu sync.Mutex
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var wg sync.WaitGroup
	r.WithSpawn(func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	})

	done := make(chan struct{})
	go func() {
		r.MaybeSweep(context.Background()) // must return while the sweep blocks
		mu.Lock()
		now = now.Add(2 * time.Hour) // due again, but one is still running
		mu.Unlock()
		r.MaybeSweep(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MaybeSweep waited for the sweep")
	}
	close(store.block)
	wg.Wait()
	if got := store.callCount(); got != 1 {
		t.Fatalf("sweeps = %d, want 1", got)
	}
}

func TestSweepJournalsOnlyChanges(t *testing.T) {
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

	r.MaybeSweep(context.Background())
	if len(events) != 0 {
		t.Fatal("a sweep that changed nothing was journaled")
	}
	store.counts = map[string]int{"events.strip": 3}
	now = now.Add(2 * time.Hour)
	r.MaybeSweep(context.Background())
	if len(events) != 1 || events[0]["counts"].(map[string]int)["events.strip"] != 3 {
		t.Fatalf("events = %v", events)
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

	forever := syncRetention(&fakeRetentionStore{}, config.RetentionConfig{}, dir, &now)
	forever.MaybeSweep(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "old.png")); err != nil {
		t.Fatal("keep-forever policy removed a file")
	}

	var journaled map[string]int
	r := syncRetention(&fakeRetentionStore{}, config.RetentionConfig{ContentDays: 7, RecordsDays: 180}, dir, &now)
	r.WithJournal(func(_ context.Context, _ string, payload map[string]any) string {
		journaled = payload["counts"].(map[string]int)
		return ""
	})
	r.MaybeSweep(context.Background())
	if journaled["attachments.delete"] != 1 {
		t.Fatalf("counts = %v", journaled)
	}
	for name, want := range map[string]bool{"old.png": false, "new.png": true, "sub": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}

	if got := (state.RetentionPolicy{Content: 7 * 24 * time.Hour, Records: 2 * 24 * time.Hour}).ContentCutoff(now); !got.Equal(now.Add(-2 * 24 * time.Hour)) {
		t.Fatalf("records shorter than content: cutoff %v", got)
	}
}
