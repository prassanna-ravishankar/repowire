package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

// Snapshots are whole-database copies the store takes before it changes
// history in a way that cannot be undone: before a schema migration (an older
// binary refuses a migrated database) and before the first retention sweep.
// One copy serves both when they coincide. Each lands next to the database as
// <db>.pre-<reason>-<UTC timestamp>, private (0600), recorded in daemon_meta
// and journaled; the store keeps the newest keepCopies of them and removes
// older ones after taking a new one, never on a timer.

// snapshotNow is the snapshot clock (a test seam).
var snapshotNow = time.Now

// DefaultKeepCopies is how many snapshots the store keeps by default.
const DefaultKeepCopies = 3

// SkipMigrationCopyEnv lets an operator migrate without the pre-migration
// copy when it cannot be taken (for example a full disk they cannot clear).
const SkipMigrationCopyEnv = "REPOWIRE_SKIP_MIGRATION_COPY"

const (
	snapshotEvent         = "state_snapshot"
	lastSnapshotKey       = "last_snapshot"
	migrationSnapshotTime = "20060102-150405"
)

// snapshotPath names a copy of dbPath taken now for reason.
func snapshotPath(dbPath, reason string, now time.Time) string {
	return dbPath + ".pre-" + reason + "-" + now.UTC().Format(migrationSnapshotTime)
}

// vacuumInto writes a consistent copy of db to path: private, and removed
// again if the copy fails part way (never a file that was there before).
func vacuumInto(ctx context.Context, db *sql.DB, path string) error {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		if info, serr := os.Stat(path); !existed && serr == nil && info.Mode().IsRegular() {
			_ = os.Remove(path) // a half-written copy is no backup
		}
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("make the copy private: %w", err)
	}
	return nil
}

// pruneSnapshots keeps the newest keep copies of dbPath and removes the rest,
// returning what it removed. keep <= 0 keeps every copy.
func pruneSnapshots(dbPath string, keep int) []string {
	if keep <= 0 {
		return nil
	}
	matches, _ := filepath.Glob(dbPath + ".pre-*")
	type copyFile struct {
		path string
		mod  time.Time
	}
	var copies []copyFile
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
			copies = append(copies, copyFile{m, info.ModTime()})
		}
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].mod.After(copies[j].mod) })
	var removed []string
	for _, c := range copies[min(keep, len(copies)):] {
		if os.Remove(c.path) == nil {
			removed = append(removed, c.path)
		}
	}
	return removed
}

// premigrationCopy copies an existing database at version from before it is
// migrated, on db (the store's only connection; nothing else runs yet). It
// returns the copy's path, or "" for a fresh database or one opened with
// SkipMigrationCopyEnv set.
func premigrationCopy(db *sql.DB, dbPath string, from int, now time.Time) (string, error) {
	if from == 0 || dbPath == "" || dbPath == ":memory:" || os.Getenv(SkipMigrationCopyEnv) != "" {
		return "", nil
	}
	path := snapshotPath(dbPath, fmt.Sprintf("migration-v%d-to-v%d", from, SchemaVersion), now)
	if err := vacuumInto(context.Background(), db, path); err != nil {
		return "", fmt.Errorf("copy the database before migrating it from schema %d to %d (set %s=1 to migrate without a copy): %w",
			from, SchemaVersion, SkipMigrationCopyEnv, err)
	}
	return path, nil
}

// markSnapshot records a copy as the last snapshot and, while no sweep has
// run, as the retention backup too (it holds all the history the first sweep
// would remove), then prunes old copies. It returns what it pruned.
func (s *Store) markSnapshot(ctx context.Context, path string) ([]string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO daemon_meta(key, value) VALUES (?, ?)`, lastSnapshotKey, path); err != nil {
		return nil, fmt.Errorf("record the snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO daemon_meta(key, value) VALUES (?, ?)`, retentionBackupKey, path); err != nil {
		return nil, fmt.Errorf("record the snapshot as the retention backup: %w", err)
	}
	return pruneSnapshots(s.path, s.keepCopies), nil
}

// recordMigrationSnapshot marks and journals the pre-migration copy.
func (s *Store) recordMigrationSnapshot(ctx context.Context, path string, from int) error {
	pruned, err := s.markSnapshot(ctx, path)
	if err != nil {
		return err
	}
	payload := map[string]any{"path": path, "reason": "migration", "from_schema": from, "to_schema": SchemaVersion}
	if len(pruned) > 0 {
		payload["pruned"] = pruned
	}
	return s.AppendEvent(ctx, peer.Event{Type: snapshotEvent, Payload: payload})
}
