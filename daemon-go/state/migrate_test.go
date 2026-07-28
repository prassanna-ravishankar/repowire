package state

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestNewStoreBootstrapsFreshDB is the keystone check: a non-existent path must
// come up fully migrated at schemaVersion, with the schema_migrations ledger
// populated — the daemon-independence guarantee (no Python pre-migration).
func TestNewStoreBootstrapsFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore fresh: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, SchemaVersion)
	}

	var ledger int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&ledger); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if ledger != len(migrationLedger) {
		t.Fatalf("schema_migrations rows = %d, want %d", ledger, len(migrationLedger))
	}

	// Every table the store reads/writes must exist and be usable.
	for _, tbl := range []string{
		"peer_session_mappings", "retired_peers", "events", "schedules",
		"session_bindings", "runtime_identity_certificates", "queued_deliveries",
		"tracked_work", "calendar_entries", "operations", "delivery_traces",
	} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Errorf("table %s not queryable: %v", tbl, err)
		}
	}

	// peer_session_mappings.model must be present (fresh CREATE, not a stale ALTER path).
	var hasModel int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('peer_session_mappings') WHERE name = 'model'`,
	).Scan(&hasModel); err != nil {
		t.Fatalf("check model column: %v", err)
	}
	if hasModel != 1 {
		t.Fatalf("peer_session_mappings.model missing")
	}
}

// TestNewStoreIdempotent verifies reopening an already-migrated DB is a no-op
// (migrate() is safe to re-run) and does not duplicate ledger rows.
func TestNewStoreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var ledger int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&ledger); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if ledger != len(migrationLedger) {
		t.Fatalf("ledger rows after reopen = %d, want %d", ledger, len(migrationLedger))
	}
}

// TestNewStoreRefusesNewerSchema guards the downgrade hazard: a DB stamped past
// SchemaVersion must be refused, not silently opened.
func TestNewStoreRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	db.Close()

	if _, err := NewStore(path); err == nil {
		t.Fatalf("expected refusal of newer schema, got nil")
	}
}
