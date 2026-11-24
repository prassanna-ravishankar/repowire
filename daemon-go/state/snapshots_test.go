package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	s.mustExec(t, `PRAGMA user_version = 17`)
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
	copies, _ := filepath.Glob(path + ".pre-migration-v17-to-v18-*")
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want one pre-migration copy", copies)
	}
	if info, err := os.Stat(copies[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, %v", info.Mode().Perm(), err)
	}
	// The copy is the database as it was: still schema 17, history included,
	// so the previous binary can be restored from it.
	ro, err := OpenReadOnly(copies[0])
	if err != nil {
		t.Fatal(err)
	}
	var version int
	_ = ro.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != 17 || ro.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-history'`) != 1 {
		t.Fatalf("copy at schema %d without its history", version)
	}
	ro.Close()
	var last, backup string
	_ = s.db.QueryRow(`SELECT value FROM daemon_meta WHERE key = 'last_snapshot'`).Scan(&last)
	_ = s.db.QueryRow(`SELECT value FROM daemon_meta WHERE key = 'retention_backup'`).Scan(&backup)
	if last != copies[0] || backup != copies[0] {
		t.Fatalf("markers: last %q, retention backup %q", last, backup)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'state_snapshot' AND json_extract(payload_json, '$.from_schema') = 17`); n != 1 {
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

func TestOldCopiesArePrunedToTheNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	olderSchemaDB(t, path)
	// A hand-made backup with a similar name is never ours to prune.
	if err := os.WriteFile(path+".pre-retention", []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	_ = os.Chtimes(path+".pre-retention", old, old)
	for i, name := range []string{"pre-migration-v15-to-v16-20261101-000000", "pre-migration-v16-to-v17-20261102-000000", "pre-retention-20261103-000000"} {
		old := path + "." + name
		if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Duration(3-i) * time.Hour)
		_ = os.Chtimes(old, at, at)
	}
	s, err := NewStoreKeeping(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	left, _ := filepath.Glob(path + ".pre-*")
	if len(left) != 3 || !slicesHasSuffix(left, ".pre-retention-20261103-000000") || !slicesHasPrefix(left, path+".pre-migration-v17-to-v18-") || !slicesHasSuffix(left, ".pre-retention") {
		t.Fatalf("kept %v, want the new copy, the newest old one, and the hand-made backup", left)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE type = 'state_snapshot' AND json_array_length(payload_json, '$.pruned') = 2`); n != 1 {
		t.Fatal("the pruned copies were not journaled")
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
	if err := os.Mkdir(snapshotPath(path, "migration-v17-to-v18", fixed), 0o700); err != nil {
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
	if version != 17 {
		t.Fatalf("the database was migrated to %d without a copy", version)
	}
	t.Setenv(SkipMigrationCopyEnv, "1")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	s.Close()
}

func slicesHasSuffix(paths []string, suffix string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

func slicesHasPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
