package state

// Schema bootstrap ported verbatim from repowire/daemon/state/database.py
// (SCHEMA_VERSION 12). The migration is a single idempotent set of
// CREATE TABLE/INDEX IF NOT EXISTS statements followed by stamping
// user_version=12 — there are no incremental ALTER migrations except the one
// backfill below (peer_session_mappings.model). Keeping the statements as one
// ordered list mirrors the Python source so the two stay diffable; when the
// Python source retires, this becomes the sole owner.

import (
	"database/sql"
	"fmt"
)

// migrationStatements are the idempotent DDL statements applied in order.
// Transcribed 1:1 from database.py migrate(); order preserved for diffability.
var migrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		description TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS legacy_imports (
		source_path TEXT PRIMARY KEY,
		source_mtime REAL,
		source_size INTEGER,
		imported_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		row_count INTEGER NOT NULL,
		status TEXT NOT NULL,
		error TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS schedules (
		schedule_id TEXT PRIMARY KEY,
		from_peer TEXT NOT NULL,
		from_peer_id TEXT,
		to_peer TEXT NOT NULL,
		to_peer_id TEXT,
		text TEXT NOT NULL,
		kind TEXT NOT NULL,
		circle TEXT,
		fire_at TEXT NOT NULL,
		cron TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		last_fired_at TEXT,
		last_outcome TEXT,
		last_error TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_schedules_fire_at ON schedules(fire_at)`,
	`CREATE TABLE IF NOT EXISTS session_bindings (
		repowire_session_id TEXT PRIMARY KEY,
		peer_id TEXT,
		current_executor_peer_id TEXT,
		backend TEXT NOT NULL,
		project_path TEXT NOT NULL,
		runtime_session_id TEXT,
		runtime_source_uri TEXT,
		source_cursor TEXT,
		provenance TEXT NOT NULL DEFAULT '{}',
		resume_capability TEXT NOT NULL DEFAULT '{}',
		status TEXT NOT NULL,
		metadata TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_session_bindings_peer ON session_bindings(peer_id)`,
	`CREATE INDEX IF NOT EXISTS idx_session_bindings_runtime ON session_bindings(backend, runtime_session_id)`,
	`CREATE INDEX IF NOT EXISTS idx_session_bindings_backend_project ON session_bindings(backend, project_path)`,
	`CREATE INDEX IF NOT EXISTS idx_session_bindings_source_uri ON session_bindings(runtime_source_uri)`,
	`CREATE TABLE IF NOT EXISTS events (
		event_id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		peer_id TEXT,
		peer_name TEXT,
		session_id TEXT,
		turn_id TEXT,
		payload_json TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_events_session_timestamp ON events(session_id, timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_events_peer_timestamp ON events(peer_id, timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_events_type_timestamp ON events(type, timestamp)`,
	`CREATE TABLE IF NOT EXISTS peer_session_mappings (
		session_id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL,
		circle TEXT NOT NULL,
		backend TEXT NOT NULL,
		path TEXT,
		role TEXT NOT NULL,
		updated_at TEXT,
		description TEXT NOT NULL DEFAULT '',
		model TEXT,
		agent_pid INTEGER,
		provenance TEXT NOT NULL DEFAULT '{}',
		description_set_at TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_peer_session_mappings_identity ON peer_session_mappings(display_name, circle, backend)`,
	`CREATE INDEX IF NOT EXISTS idx_peer_session_mappings_path ON peer_session_mappings(backend, path)`,
	`CREATE TABLE IF NOT EXISTS runtime_identity_certificates (
		nonce TEXT PRIMARY KEY,
		peer_id TEXT NOT NULL,
		display_name TEXT NOT NULL,
		backend TEXT NOT NULL,
		project_path TEXT NOT NULL,
		runtime_session_id TEXT,
		pane_id TEXT,
		agent_pid INTEGER,
		parent_pid INTEGER,
		issued_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		metadata TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_peer ON runtime_identity_certificates(peer_id)`,
	`CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_runtime ON runtime_identity_certificates(backend, runtime_session_id)`,
	`CREATE TABLE IF NOT EXISTS queued_deliveries (
		delivery_id TEXT PRIMARY KEY,
		peer_id TEXT NOT NULL,
		repowire_session_id TEXT,
		kind TEXT NOT NULL,
		from_peer_id TEXT,
		from_peer_name TEXT NOT NULL,
		to_peer_name TEXT NOT NULL,
		correlation_id TEXT,
		text TEXT NOT NULL,
		attachments_json TEXT NOT NULL DEFAULT '[]',
		metadata_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tracked_work (
		work_id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT 'general',
		state TEXT NOT NULL,
		state_reason TEXT,
		phase TEXT,
		progress_json TEXT NOT NULL DEFAULT '{}',
		progress_events_json TEXT NOT NULL DEFAULT '[]',
		owner_peer_id TEXT,
		assigned_peer_id TEXT,
		repowire_session_id TEXT,
		correlation_id TEXT,
		circle TEXT,
		created_by_peer_id TEXT,
		source_kind TEXT,
		source_id TEXT,
		scope TEXT,
		visibility TEXT NOT NULL DEFAULT 'circle',
		request_json TEXT NOT NULL DEFAULT '{}',
		deadline_at TEXT,
		expires_at TEXT,
		result_summary TEXT,
		result_data_json TEXT NOT NULL DEFAULT '{}',
		error_json TEXT NOT NULL DEFAULT '{}',
		artifacts_json TEXT NOT NULL DEFAULT '[]',
		provenance_json TEXT NOT NULL DEFAULT '{}',
		cancel_requested INTEGER NOT NULL DEFAULT 0,
		cancel_requested_at TEXT,
		cancel_requested_by_peer_id TEXT,
		cancellation_reason TEXT,
		completed_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_queued_deliveries_peer_created ON queued_deliveries(peer_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_queued_deliveries_expires ON queued_deliveries(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_tracked_work_state ON tracked_work(state)`,
	`CREATE INDEX IF NOT EXISTS idx_tracked_work_owner_updated ON tracked_work(owner_peer_id, updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_tracked_work_session_updated ON tracked_work(repowire_session_id, updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_tracked_work_circle_updated ON tracked_work(circle, updated_at)`,
	`CREATE TABLE IF NOT EXISTS calendar_entries (
		calendar_id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		kind TEXT NOT NULL,
		state TEXT NOT NULL,
		cron TEXT NOT NULL,
		next_due_at TEXT NOT NULL,
		owner_peer_id TEXT,
		assigned_peer_id TEXT,
		circle TEXT,
		created_by_peer_id TEXT,
		source_kind TEXT,
		source_id TEXT,
		scope TEXT,
		visibility TEXT NOT NULL,
		request_json TEXT NOT NULL,
		provenance_json TEXT NOT NULL,
		last_occurrence_work_id TEXT,
		last_materialized_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_calendar_entries_state_due ON calendar_entries(state, next_due_at)`,
	`CREATE INDEX IF NOT EXISTS idx_calendar_entries_circle_due ON calendar_entries(circle, next_due_at)`,
	`CREATE TABLE IF NOT EXISTS operations (
		operation_id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		state TEXT NOT NULL,
		target_json TEXT NOT NULL DEFAULT '{}',
		strategy TEXT,
		attempts_json TEXT NOT NULL DEFAULT '[]',
		result_json TEXT NOT NULL DEFAULT '{}',
		error_json TEXT NOT NULL DEFAULT '{}',
		provenance_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		completed_at TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_operations_kind_updated ON operations(kind, updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_operations_state_updated ON operations(state, updated_at)`,
	`CREATE TABLE IF NOT EXISTS delivery_traces (
		id TEXT PRIMARY KEY,
		trace_id TEXT NOT NULL,
		delivery_id TEXT,
		seq INTEGER NOT NULL,
		kind TEXT NOT NULL,
		stage TEXT NOT NULL,
		status TEXT NOT NULL,
		peer_id TEXT,
		from_peer_id TEXT,
		ts TEXT NOT NULL,
		detail_json TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_traces_trace ON delivery_traces(trace_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_traces_peer_stage ON delivery_traces(peer_id, stage, ts DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_traces_ts ON delivery_traces(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_traces_stage_ts ON delivery_traces(stage, ts)`,
	`CREATE TABLE IF NOT EXISTS retired_peers (
		peer_id TEXT PRIMARY KEY,
		retired_at TEXT NOT NULL,
		hard INTEGER NOT NULL DEFAULT 0
	)`,
	// Human inbox (v16): one row per item, read/replied state once for every
	// surface, and one push record per (item, surface).
	`CREATE TABLE IF NOT EXISTS inbox_items (
		id TEXT PRIMARY KEY,
		direction TEXT NOT NULL,
		kind TEXT NOT NULL,
		trace_id TEXT NOT NULL,
		trace_kind TEXT NOT NULL,
		from_peer_id TEXT,
		from_name TEXT NOT NULL DEFAULT '',
		to_peer_id TEXT,
		to_name TEXT NOT NULL DEFAULT '',
		correlation_id TEXT,
		priority INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL DEFAULT '',
		attachments_json TEXT NOT NULL DEFAULT '[]',
		question_json TEXT,
		created_at TEXT NOT NULL,
		read_at TEXT,
		read_surface TEXT,
		replied_at TEXT,
		reply_surface TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_inbox_items_unread ON inbox_items(read_at, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_inbox_items_cid ON inbox_items(correlation_id)`,
	`CREATE TABLE IF NOT EXISTS inbox_pushes (
		item_id TEXT NOT NULL,
		surface TEXT NOT NULL,
		status TEXT NOT NULL,
		error_class TEXT,
		surface_ref TEXT,
		ts TEXT NOT NULL,
		PRIMARY KEY (item_id, surface)
	)`,
	`CREATE TABLE IF NOT EXISTS inbox_refs (
		surface TEXT NOT NULL,
		ref TEXT NOT NULL,
		item_id TEXT NOT NULL,
		PRIMARY KEY (surface, ref)
	)`,
	// Cross-machine asks and notifies (v17, rfcs/cross-machine-circles.md).
	// Times are unix seconds. store_epoch in daemon_meta names this database.
	`CREATE TABLE IF NOT EXISTS daemon_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS remote_inbound (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		from_machine TEXT NOT NULL,
		from_peer_id TEXT NOT NULL,
		from_name TEXT NOT NULL,
		from_circle TEXT NOT NULL,
		to_peer_id TEXT NOT NULL,
		text TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		state TEXT NOT NULL,
		close_reason TEXT NOT NULL DEFAULT '',
		reply TEXT,
		settled_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_remote_inbound_state ON remote_inbound(state)`,
	`CREATE TABLE IF NOT EXISTS remote_fences (
		id TEXT NOT NULL,
		from_machine TEXT NOT NULL,
		from_peer_id TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (id, from_machine)
	)`,
	`CREATE TABLE IF NOT EXISTS remote_outbound (
		id TEXT PRIMARY KEY,
		machine TEXT NOT NULL,
		store_epoch TEXT NOT NULL,
		to_peer_id TEXT NOT NULL,
		to_name TEXT NOT NULL,
		from_peer_id TEXT NOT NULL,
		from_name TEXT NOT NULL,
		from_circle TEXT NOT NULL,
		text TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		state TEXT NOT NULL,
		close_reason TEXT NOT NULL DEFAULT '',
		reply TEXT,
		delivered INTEGER NOT NULL DEFAULT 0,
		checked_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_remote_outbound_asker ON remote_outbound(from_peer_id, state)`,
}

// migrationLedger mirrors the schema_migrations rows database.py stamps, so a
// Go-created DB is byte-compatible with a Python-created one for tooling that
// reads the ledger.
var migrationLedger = []struct {
	version     int
	description string
}{
	{1, "initial daemon state schema with schedules"},
	{2, "session bindings for runtime provenance metadata"},
	{3, "dashboard event journal"},
	{4, "peer session mappings for PeerRegistry identity state"},
	{5, "daemon-minted runtime identity birth certificates"},
	{6, "queued deliveries for polling peers"},
	{7, "tracked work lifecycle records"},
	{8, "recurring durable job calendar entries"},
	{9, "durable operation lifecycle records"},
	{10, "delivery trace ledger"},
	{11, "observed peer runtime model"},
	{12, "retired peer identities survive daemon restarts"},
	{13, "operator-closed peer identities reject live-runtime reclaim"},
	{14, "peer provenance: source, parent runtime id, ephemeral, addressability"},
	{15, "description_set_at for claim staleness; route_decision trace index"},
	{16, "human inbox items, per-surface push records and rendered refs"},
	{17, "cross-machine asks: store epoch, inbound and outbound records, fences"},
}

// migrate applies the idempotent schema and stamps user_version. Safe to run on
// a fresh or already-current DB. Mirrors database.py migrate() exactly.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range migrationStatements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migration statement %d: %w", i, err)
		}
	}

	// Backfill for pre-model DBs: database.py ALTERs peer_session_mappings to add
	// `model` when absent. On a fresh DB the CREATE above already has it, so guard
	// against the "duplicate column" error rather than probing table_info.
	if err := ensureModelColumn(tx); err != nil {
		return err
	}
	if err := ensureProvenanceColumn(tx); err != nil {
		return err
	}
	if err := ensureRetiredHardColumn(tx); err != nil {
		return err
	}
	if err := ensureDescriptionSetAtColumn(tx); err != nil {
		return err
	}

	for _, m := range migrationLedger {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO schema_migrations(version, description) VALUES (?, ?)`,
			m.version, m.description,
		); err != nil {
			return fmt.Errorf("stamp migration %d: %w", m.version, err)
		}
	}

	// PRAGMA user_version does not accept a bind parameter.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return tx.Commit()
}

func ensureRetiredHardColumn(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('retired_peers') WHERE name = 'hard'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("check retired hard column: %w", err)
	}
	if count == 0 {
		if _, err := tx.Exec(`ALTER TABLE retired_peers ADD COLUMN hard INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add retired hard column: %w", err)
		}
	}
	return nil
}

// ensureModelColumn adds peer_session_mappings.model on legacy DBs that predate
// it. Fresh DBs already have the column (in the CREATE above); the ALTER then
// errors with "duplicate column name", which we treat as already-present.
func ensureModelColumn(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('peer_session_mappings') WHERE name = 'model'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("check model column: %w", err)
	}
	if count == 0 {
		if _, err := tx.Exec(`ALTER TABLE peer_session_mappings ADD COLUMN model TEXT`); err != nil {
			return fmt.Errorf("add model column: %w", err)
		}
	}
	return nil
}

// ensureProvenanceColumn adds peer_session_mappings.provenance (migration 14)
// to databases created before it. The '{}' default reads back as
// proto.DefaultProvenance: source unknown, addressable. Nothing is inferred
// here; the registry fills unknown from live registration evidence.
func ensureProvenanceColumn(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('peer_session_mappings') WHERE name = 'provenance'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("check provenance column: %w", err)
	}
	if count == 0 {
		if _, err := tx.Exec(`ALTER TABLE peer_session_mappings ADD COLUMN provenance TEXT NOT NULL DEFAULT '{}'`); err != nil {
			return fmt.Errorf("add provenance column: %w", err)
		}
	}
	return nil
}

func ensureDescriptionSetAtColumn(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('peer_session_mappings') WHERE name = 'description_set_at'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("check description_set_at column: %w", err)
	}
	if count == 0 {
		if _, err := tx.Exec(`ALTER TABLE peer_session_mappings ADD COLUMN description_set_at TEXT`); err != nil {
			return fmt.Errorf("add description_set_at column: %w", err)
		}
	}
	return nil
}
