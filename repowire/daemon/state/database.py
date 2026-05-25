"""SQLite state database lifecycle and migrations."""

from __future__ import annotations

import logging
import sqlite3
from pathlib import Path

logger = logging.getLogger(__name__)

SCHEMA_VERSION = 7


class StateDatabase:
    """Small daemon-owned SQLite connection wrapper."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        try:
            self.path.parent.chmod(0o700)
        except OSError:
            logger.debug("Could not chmod state directory %s", self.path.parent, exc_info=True)
        self.conn = sqlite3.connect(str(path), check_same_thread=False)
        self.conn.row_factory = sqlite3.Row
        self._apply_pragmas()
        self.migrate()

    def _apply_pragmas(self) -> None:
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA synchronous=NORMAL")
        self.conn.execute("PRAGMA foreign_keys=ON")
        self.conn.execute("PRAGMA busy_timeout=5000")

    def migrate(self) -> None:
        """Apply idempotent schema migrations."""
        with self.conn:
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS schema_migrations (
                    version INTEGER PRIMARY KEY,
                    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
                    description TEXT NOT NULL
                )
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS legacy_imports (
                    source_path TEXT PRIMARY KEY,
                    source_mtime REAL,
                    source_size INTEGER,
                    imported_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
                    row_count INTEGER NOT NULL,
                    status TEXT NOT NULL,
                    error TEXT
                )
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS schedules (
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
                )
                """,
            )
            self.conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_schedules_fire_at ON schedules(fire_at)",
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS session_bindings (
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
                )
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_session_bindings_peer
                ON session_bindings(peer_id)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_session_bindings_runtime
                ON session_bindings(backend, runtime_session_id)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_session_bindings_backend_project
                ON session_bindings(backend, project_path)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_session_bindings_source_uri
                ON session_bindings(runtime_source_uri)
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS events (
                    event_id TEXT PRIMARY KEY,
                    type TEXT NOT NULL,
                    timestamp TEXT NOT NULL,
                    peer_id TEXT,
                    peer_name TEXT,
                    session_id TEXT,
                    turn_id TEXT,
                    payload_json TEXT NOT NULL
                )
                """,
            )
            self.conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp)",
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_events_session_timestamp
                ON events(session_id, timestamp)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_events_peer_timestamp
                ON events(peer_id, timestamp)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_events_type_timestamp
                ON events(type, timestamp)
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS peer_session_mappings (
                    session_id TEXT PRIMARY KEY,
                    display_name TEXT NOT NULL,
                    circle TEXT NOT NULL,
                    backend TEXT NOT NULL,
                    path TEXT,
                    role TEXT NOT NULL,
                    updated_at TEXT,
                    description TEXT NOT NULL DEFAULT '',
                    agent_pid INTEGER
                )
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_peer_session_mappings_identity
                ON peer_session_mappings(display_name, circle, backend)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_peer_session_mappings_path
                ON peer_session_mappings(backend, path)
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS runtime_identity_certificates (
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
                )
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_peer
                ON runtime_identity_certificates(peer_id)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_runtime
                ON runtime_identity_certificates(backend, runtime_session_id)
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS queued_deliveries (
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
                )
                """,
            )
            self.conn.execute(
                """
                CREATE TABLE IF NOT EXISTS tracked_work (
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
                )
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_queued_deliveries_peer_created
                ON queued_deliveries(peer_id, created_at)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_queued_deliveries_expires
                ON queued_deliveries(expires_at)
                """,
            )
            self.conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_tracked_work_state ON tracked_work(state)",
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_tracked_work_owner_updated
                ON tracked_work(owner_peer_id, updated_at)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_tracked_work_session_updated
                ON tracked_work(repowire_session_id, updated_at)
                """,
            )
            self.conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_tracked_work_circle_updated
                ON tracked_work(circle, updated_at)
                """,
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (1, "initial daemon state schema with schedules"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (2, "session bindings for runtime provenance metadata"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (3, "dashboard event journal"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (4, "peer session mappings for PeerRegistry identity state"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (5, "daemon-minted runtime identity birth certificates"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (6, "queued deliveries for polling peers"),
            )
            self.conn.execute(
                """
                INSERT OR IGNORE INTO schema_migrations(version, description)
                VALUES (?, ?)
                """,
                (7, "tracked work lifecycle records"),
            )
            self.conn.execute(f"PRAGMA user_version={SCHEMA_VERSION}")

    def integrity_check(self) -> str:
        row = self.conn.execute("PRAGMA integrity_check").fetchone()
        return str(row[0]) if row is not None else "missing"

    def close(self) -> None:
        try:
            self.conn.execute("PRAGMA wal_checkpoint(PASSIVE)")
        except sqlite3.Error:
            logger.debug("SQLite WAL checkpoint failed", exc_info=True)
        self.conn.close()
