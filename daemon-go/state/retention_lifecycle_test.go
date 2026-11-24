package state

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

// Every table declares its retention beside its queries, or why it is
// exempt; a new table fails here until its owner decides.
func TestEveryTableDeclaresRetention(t *testing.T) {
	s := newSweepStore(t)
	declared := map[string]int{}
	for _, d := range tableRetentions {
		if (d.rules == nil) == (d.exempt == "") {
			t.Errorf("%v: declare rules or an exemption, not both or neither", d.tables)
		}
		for _, table := range d.tables {
			declared[table]++
		}
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = true
		if declared[name] != 1 {
			t.Errorf("table %s has %d retention declarations, want 1", name, declared[name])
		}
	}
	for table := range declared {
		if !tables[table] {
			t.Errorf("retention declared for %s, which is not a table", table)
		}
	}
}

// waitSweep waits until no sweep is running.
func waitSweep(t *testing.T, r *retention) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sweep still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStoreSweepsAtStartThenAtMostHourlyOnWrites(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	var mu sync.Mutex
	now := sweepNow
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	transcripts := func() int { return s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'chat_turn'`) }

	seedEvent(t, s, "e-old-1", "chat_turn", ago(8*day), `{"text":"x"}`)
	r := &retention{policy: defaultPolicy, now: clock}
	s.startRetention(r)
	waitSweep(t, r)
	if n := transcripts(); n != 0 {
		t.Fatalf("start sweep left %d transcripts", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'retention_swept' AND json_extract(payload_json, '$.counts."events.delete_transcripts"') = 1`); n != 1 {
		t.Fatalf("retention_swept events = %d, want 1", n)
	}

	seedEvent(t, s, "e-old-2", "chat_turn", ago(8*day), `{"text":"x"}`)
	advance(59 * time.Minute)
	if err := s.AppendEvent(ctx, peer.Event{Type: "peer_status"}); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if n := transcripts(); n != 1 {
		t.Fatalf("a write within the hour swept (transcripts %d)", n)
	}
	advance(2 * time.Minute)
	if err := s.AppendEvent(ctx, peer.Event{Type: "peer_status"}); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if n := transcripts(); n != 0 {
		t.Fatalf("a write after the hour did not sweep (transcripts %d)", n)
	}
}

// Attachment files plug in as a pass, the journal entry goes out through the
// daemon's event path, and a store has one retention lifecycle.
func TestRetentionPlugsAndASingleLifecycle(t *testing.T) {
	s := newSweepStore(t)
	seedEvent(t, s, "e-old", "chat_turn", ago(8*day), `{"text":"x"}`)
	var mu sync.Mutex
	var emitted []map[string]any
	toAny := func(m map[string]int) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	r := &retention{policy: defaultPolicy, now: func() time.Time { return sweepNow }, plugs: RetentionPlugs{
		Passes: []func(context.Context, RetentionPolicy, time.Time) map[string]int{
			func(_ context.Context, p RetentionPolicy, now time.Time) map[string]int {
				if !p.ContentCutoff(now).Equal(sweepNow.Add(-7 * day)) {
					t.Errorf("pass got cutoff %v", p.ContentCutoff(now))
				}
				return map[string]int{"attachments.delete": 2}
			},
		},
		Emit: func(_ context.Context, typ string, payload map[string]any) string {
			mu.Lock()
			defer mu.Unlock()
			if typ == "retention_swept" {
				emitted = append(emitted, toAny(payload["counts"].(map[string]int)))
			}
			return ""
		},
	}}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	mu.Lock()
	if len(emitted) != 1 || emitted[0]["attachments.delete"] != 2 || emitted[0]["events.delete_transcripts"] != 1 {
		t.Fatalf("emitted %v", emitted)
	}
	mu.Unlock()
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'retention_swept'`); n != 0 {
		t.Fatalf("with Emit plugged, the store also journaled %d", n)
	}
	if err := s.StartRetention(defaultPolicy, RetentionPlugs{}); err != ErrRetentionStarted {
		t.Fatalf("second start: %v", err)
	}
}

// A sweep that changed nothing journals nothing, even when a pass reports
// zero.
func TestASweepThatChangesNothingIsNotJournaled(t *testing.T) {
	s := newSweepStore(t)
	emitted := 0
	r := &retention{policy: defaultPolicy, now: func() time.Time { return sweepNow }, plugs: RetentionPlugs{
		Passes: []func(context.Context, RetentionPolicy, time.Time) map[string]int{
			func(context.Context, RetentionPolicy, time.Time) map[string]int {
				return map[string]int{"attachments.delete": 0}
			},
		},
		Emit: func(_ context.Context, typ string, _ map[string]any) string {
			if typ == "retention_swept" { // the first sweep also journals its backup
				emitted++
			}
			return ""
		},
	}}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if emitted != 0 {
		t.Fatalf("an empty sweep was journaled %d times", emitted)
	}
}

// Close cancels a sweep in flight and returns only after it ends, so the db
// never closes under it (#572); no sweep starts afterwards.
func TestCloseWaitsForARunningSweep(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan struct{})
	var cancelled bool
	r := &retention{policy: defaultPolicy, now: time.Now, beforeSweep: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		cancelled = true
		time.Sleep(50 * time.Millisecond) // a slow batch finishing
		close(ended)
	}}
	s.startRetention(r)
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("Close returned before the sweep ended")
	}
	if !cancelled {
		t.Fatal("Close did not cancel the sweep")
	}
	r.mu.Lock()
	r.due = time.Time{}
	r.mu.Unlock()
	s.maybeSweep()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		t.Fatal("a sweep started after Close")
	}
}

// An open ask still reads its inbox item, peer message, refs and trace, so
// nothing younger than the ask lifetime expires, however short the policy.
func TestNothingYoungerThanAnOpenAskExpires(t *testing.T) {
	s := newSweepStore(t)
	insert := func(id, kind, created string) {
		s.mustExec(t, `INSERT INTO inbox_items(id, direction, kind, trace_id, trace_kind, text, created_at)
			VALUES (?, 'in', ?, ?, ?, 'secret', ?)`, id, kind, id, kind, created)
		s.mustExec(t, `INSERT INTO inbox_refs(surface, ref, item_id) VALUES ('telegram', ?, ?)`, id, id)
		s.mustExec(t, `INSERT INTO peer_messages(id, kind, peer_id, text, created_at) VALUES (?, ?, 'repow-b', 'secret', ?)`, "msg-"+id, kind, created)
	}
	insert("ask-open", "ask", ago(2*day))
	insert("ask-expired", "ask", ago(4*day))
	short := RetentionPolicy{Content: day, AskLife: 3 * day}

	sweep(t, s, short)
	for id, kept := range map[string]bool{"ask-open": true, "ask-expired": false} {
		if got := s.count(t, `SELECT COUNT(*) FROM inbox_items WHERE id = ? AND text = 'secret'`, id) == 1; got != kept {
			t.Errorf("inbox %s keeps text = %v, want %v", id, got, kept)
		}
		if got := s.count(t, `SELECT COUNT(*) FROM peer_messages WHERE id = ?`, "msg-"+id) == 1; got != kept {
			t.Errorf("message %s kept = %v, want %v", id, got, kept)
		}
	}

	short.Records = day
	sweep(t, s, short)
	for id, kept := range map[string]bool{"ask-open": true, "ask-expired": false} {
		if got := s.count(t, `SELECT COUNT(*) FROM inbox_items WHERE id = ?`, id) == 1; got != kept {
			t.Errorf("inbox %s kept = %v, want %v", id, got, kept)
		}
		if got := s.count(t, `SELECT COUNT(*) FROM inbox_refs WHERE item_id = ?`, id) == 1; got != kept {
			t.Errorf("ref %s kept = %v, want %v", id, got, kept)
		}
	}
}

// Before a database's first sweep the store copies it once, next to it, so
// an upgrade that turns retention on never loses history beyond recovery.
func TestFirstSweepBacksUpTheDatabaseOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mu sync.Mutex
	now := sweepNow
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	seedEvent(t, s, "e-old", "chat_turn", ago(8*day), `{"text":"history"}`)

	r := &retention{policy: defaultPolicy, now: clock}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	backups, _ := filepath.Glob(dbPath + ".pre-retention-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one", backups)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-old'`); n != 0 {
		t.Fatal("the sweep did not run after the backup")
	}
	if info, err := os.Stat(backups[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v; want 0600 (it holds message text)", info.Mode().Perm(), err)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'retention_backup' AND json_extract(payload_json, '$.path') = ?`, backups[0]); n != 1 {
		t.Fatal("the backup's path was not journaled")
	}
	copyStore, err := NewStore(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if n := copyStore.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-old'`); n != 1 {
		t.Fatal("the backup does not hold what the sweep removed")
	}
	copyStore.Close()

	// A later sweep takes no second copy.
	seedEvent(t, s, "e-old-2", "chat_turn", ago(8*day), `{"text":"x"}`)
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if err := s.AppendEvent(ctx, peer.Event{Type: "peer_status"}); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if again, _ := filepath.Glob(dbPath + ".pre-retention-*"); len(again) != 1 {
		t.Fatalf("backups after a second sweep = %v", again)
	}
}

func TestNoBackupWhenRetentionKeepsEverything(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := &retention{policy: RetentionPolicy{}, now: func() time.Time { return sweepNow }}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if backups, _ := filepath.Glob(dbPath + ".pre-retention-*"); len(backups) != 0 {
		t.Fatalf("keep-forever policy took %v", backups)
	}
}

// A backup that fails stops the sweep: nothing is removed without a copy.
func TestFailedBackupMeansNoSweep(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedEvent(t, s, "e-old", "chat_turn", ago(8*day), `{"text":"history"}`)
	// Occupy the path the copy would take, so VACUUM INTO fails.
	if err := os.Mkdir(dbPath+".pre-retention-"+sweepNow.UTC().Format("20060102-150405"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &retention{policy: defaultPolicy, now: func() time.Time { return sweepNow }}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-old'`); n != 1 {
		t.Fatal("the sweep ran without a backup")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'retention_backup_failed'`); n != 1 {
		t.Fatal("the failed backup was not journaled")
	}
}
