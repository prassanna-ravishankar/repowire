"""SQLite state database lifecycle and migrations."""

from __future__ import annotations

import logging
import sqlite3
from pathlib import Path

logger = logging.getLogger(__name__)

SCHEMA_VERSION = 2


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
