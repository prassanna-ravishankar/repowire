package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// olderSchema is the schema the copy tests migrate from.
const olderSchema = SchemaVersion - 1

// olderSchemaDB leaves a database at path that looks one schema behind, with
// one event in it.
func olderSchemaDB(t *testing.T, path string) {
	t.Helper()
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seedEvent(t, s, "e-history", "chat_turn", ago(day), `{"text":"history"}`)
	s.mustExec(t, `DELETE FROM daemon_meta WHERE key IN ('last_snapshot', 'retention_backup')`)
	s.mustExec(t, fmt.Sprintf(`PRAGMA user_version = %d`, olderSchema))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationCopiesTheOlderDatabaseFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	olderSchemaDB(t, path)
	s, err := NewStoreKeeping(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	copies, _ := filepath.Glob(path + fmt.Sprintf(".pre-migration-v%d-to-v%d-*", olderSchema, SchemaVersion))
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want one pre-migration copy", copies)
	}
	if info, err := os.Stat(copies[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, %v", info.Mode().Perm(), err)
	}
	// The copy is the database as it was: still the older schema, history included,
	// so the previous binary can be restored from it.
	ro, err := OpenReadOnly(copies[0])
	if err != nil {
		t.Fatal(err)
	}
	var version int
	_ = ro.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != olderSchema || ro.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-history'`) != 1 {
		t.Fatalf("copy at schema %d without its history", version)
	}
	ro.Close()
	var last, backup string
	_ = s.db.QueryRow(`SELECT value FROM daemon_meta WHERE key = 'last_snapshot'`).Scan(&last)
	_ = s.db.QueryRow(`SELECT value FROM daemon_meta WHERE key = 'retention_backup'`).Scan(&backup)
	if last != copies[0] || backup != copies[0] {
		t.Fatalf("markers: last %q, retention backup %q", last, backup)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'state_snapshot' AND json_extract(payload_json, '$.from_schema') = ?`, olderSchema); n != 1 {
		t.Fatal("the copy was not journaled")
	}

	// The first sweep finds its backup already taken: no second copy.
	r := &retention{policy: defaultPolicy, now: func() time.Time { return sweepNow }}
	if err := s.startRetention(r); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, r)
	if all, _ := filepath.Glob(path + ".pre-*"); len(all) != 1 {
		t.Fatalf("copies after the first sweep = %v", all)
	}
}

func TestFreshDatabaseIsNotCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if all, _ := filepath.Glob(path + ".pre-*"); len(all) != 0 {
		t.Fatalf("a fresh database was copied: %v", all)
	}
}

// Pruning removes only copies the store recorded, newest by name kept: a
// user's own backups, even ones named like the store's, are never touched.
func TestOldCopiesArePrunedToTheNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	olderSchemaDB(t, path)
	recorded := []string{path + ".pre-migration-v15-to-v16-20261101-000000", path + ".pre-migration-v16-to-v17-20261102-000000", path + ".pre-retention-20261103-000000"}
	users := []string{path + ".pre-foo", path + ".pre-retention", path + ".pre-retention-20250101-000000"}
	for _, p := range append(append([]string{}, recorded...), users...) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Record the earlier copies as the store records its own.
	s0, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	s0.Close()
	raw, _ := json.Marshal(recorded)
	rw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT OR REPLACE INTO daemon_meta(key, value) VALUES ('snapshots', ?), ('retention_backup', ?)`, string(raw), recorded[2]); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	s, err := NewStoreKeeping(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for _, p := range users {
		if !exists(p) {
			t.Fatalf("a file the store did not record was removed: %s", p)
		}
	}
	if exists(recorded[0]) || exists(recorded[1]) {
		t.Fatal("older recorded copies were kept beyond retention.copies")
	}
	if !exists(recorded[2]) {
		t.Fatal("the retention backup was pruned")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'state_snapshot' AND json_array_length(payload_json, '$.pruned') = 2`); n != 1 {
		t.Fatal("the pruned copies were not journaled")
	}
}

// The retention backup is exempt, and names, not mtimes, order the rest.
func TestPruningKeepsTheRetentionBackup(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "state.db.pre-retention-20260101-000000")
	older := filepath.Join(dir, "state.db.pre-migration-v15-to-v16-20260201-000000")
	newer := filepath.Join(dir, "state.db.pre-migration-v16-to-v17-20260301-000000")
	for _, p := range []string{backup, older, newer} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(older, time.Now(), time.Now()) // mtime says newest; the name says older
	_ = os.Chtimes(newer, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	kept, removed := pruneSnapshots([]string{backup, older, newer}, 1, backup)
	if len(removed) != 1 || removed[0] != older || len(kept) != 2 {
		t.Fatalf("kept %v removed %v, want only %s removed", kept, removed, older)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("the retention backup was removed")
	}
}

// A copy that cannot be taken refuses the migration: an operator who must
// migrate anyway sets SkipMigrationCopyEnv.
func TestFailedMigrationCopyRefusesTheMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	olderSchemaDB(t, path)
	fixed := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	snapshotNow = func() time.Time { return fixed }
	t.Cleanup(func() { snapshotNow = time.Now })
	if err := os.Mkdir(snapshotPath(path, fmt.Sprintf("migration-v%d-to-v%d", olderSchema, SchemaVersion), fixed), 0o700); err != nil {
		t.Fatal(err) // occupy the copy's path
	}
	if _, err := NewStore(path); err == nil || !strings.Contains(err.Error(), SkipMigrationCopyEnv) {
		t.Fatalf("migration without a copy: %v", err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	_ = ro.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	ro.Close()
	if version != olderSchema {
		t.Fatalf("the database was migrated to %d without a copy", version)
	}
	t.Setenv(SkipMigrationCopyEnv, "1")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	s.Close()
}
