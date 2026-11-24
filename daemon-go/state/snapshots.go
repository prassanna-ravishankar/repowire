package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

// Snapshots are whole-database copies the store takes before it changes
// history in a way that cannot be undone: before a schema migration (an older
// binary refuses a migrated database) and before the first retention sweep.
// One copy serves both when they coincide. Each lands next to the database as
// <db>.pre-<reason>-<UTC timestamp>, private (0600), recorded by exact path in
// daemon_meta and journaled. The store keeps the newest keepCopies of the
// copies it recorded and removes older ones after taking a new one, never on
// a timer and never the retention backup.

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

// snapshotsKey in daemon_meta lists, as a JSON array, the exact paths of
// the copies this store took. Pruning considers only those: a file the store
// did not create and record (a hand-made state.db.pre-anything) is never
// touched.
const snapshotsKey = "snapshots"

// snapshotStamp is the timestamp in a copy's name, which orders copies (a
// copy or touch can change a file's mtime, not its name).
var snapshotStamp = regexp.MustCompile(`-([0-9]{8}-[0-9]{6})$`)

// pruneSnapshots keeps the newest keep of the recorded copies, by the
// timestamp in their names, and removes the rest from disk. protect (the
// retention backup: the only pre-retention history) is never removed and does
// not count toward keep. It returns the copies still recorded and those it
// removed. keep <= 0 keeps every copy.
func pruneSnapshots(recorded []string, keep int, protect string) (kept, removed []string) {
	if keep <= 0 {
		return recorded, nil
	}
	var candidates []string
	for _, p := range recorded {
		if p == protect {
			kept = append(kept, p)
			continue
		}
		candidates = append(candidates, p)
	}
	stamp := func(p string) string {
		if m := snapshotStamp.FindStringSubmatch(p); m != nil {
			return m[1]
		}
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return stamp(candidates[i]) > stamp(candidates[j]) })
	cut := min(keep, len(candidates))
	kept = append(kept, candidates[:cut]...)
	for _, p := range candidates[cut:] {
		if err := os.Remove(p); err == nil || os.IsNotExist(err) {
			removed = append(removed, p)
		} else {
			kept = append(kept, p) // could not remove: still ours, try next time
		}
	}
	return kept, removed
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
// would remove), then prunes old copies, never the retention backup. It
// returns what it pruned.
//
// The retention backup marker is absent only on a database no released
// binary has swept: no release swept before the marker existed (#566 landed
// after v0.21.1). A dev build from main between #566 and #595 may have swept
// without it; there the first copy is labelled the backup though that history
// was already gone. Nothing more is lost.
func (s *Store) markSnapshot(ctx context.Context, path string) ([]string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO daemon_meta(key, value) VALUES (?, ?)`, lastSnapshotKey, path); err != nil {
		return nil, fmt.Errorf("record the snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO daemon_meta(key, value) VALUES (?, ?)`, retentionBackupKey, path); err != nil {
		return nil, fmt.Errorf("record the snapshot as the retention backup: %w", err)
	}
	var backup, raw string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM daemon_meta WHERE key = ?`, retentionBackupKey).Scan(&backup)
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM daemon_meta WHERE key = ?`, snapshotsKey).Scan(&raw)
	var recorded []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &recorded); err != nil {
			return nil, fmt.Errorf("read the snapshot list: %w", err)
		}
	}
	recorded = append(recorded, path)
	kept, removed := pruneSnapshots(recorded, s.keepCopies, backup)
	encoded, _ := json.Marshal(kept)
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO daemon_meta(key, value) VALUES (?, ?)`, snapshotsKey, string(encoded)); err != nil {
		return removed, fmt.Errorf("record the snapshot list: %w", err)
	}
	return removed, nil
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
