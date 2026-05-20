"""SQLite-backed schedule persistence adapter."""

from __future__ import annotations

import json
import logging
import sqlite3
from dataclasses import asdict
from datetime import datetime, timezone
from pathlib import Path
from uuid import uuid4

from repowire.daemon.schedule_cron import next_fire_after, validate_cron
from repowire.daemon.schedule_store import _VALID_KINDS, Schedule, ScheduleKind
from repowire.daemon.state.database import StateDatabase

logger = logging.getLogger(__name__)


class SQLiteScheduleStore:
    """ScheduleStore-compatible adapter backed by the daemon state DB."""

    def __init__(self, db: StateDatabase, legacy_path: Path | None = None) -> None:
        self._db = db
        self._conn = db.conn
        if legacy_path is not None:
            self._import_legacy_if_empty(legacy_path)

    def _import_legacy_if_empty(self, legacy_path: Path) -> None:
        existing = self._conn.execute("SELECT COUNT(*) FROM schedules").fetchone()
        if existing is not None and int(existing[0]) > 0:
            return
        if not legacy_path.exists():
            return
        try:
            raw_data = json.loads(legacy_path.read_text())
            if not isinstance(raw_data, dict):
                raise TypeError("top-level schedules JSON must be an object")
            schedules = [Schedule(**raw) for raw in raw_data.values()]
        except (json.JSONDecodeError, TypeError, ValueError, KeyError) as e:
            logger.error("Corrupt legacy schedules file (%s); skipping SQLite import", e)
            with self._conn:
                self._record_import(legacy_path, 0, "error", type(e).__name__)
            return
        imported = 0
        with self._conn:
            for sched in schedules:
                self._upsert_schedule(sched)
                imported += 1
            self._record_import(legacy_path, imported, "ok", None)
        logger.info("Imported %d legacy schedules into SQLite state", imported)

    def _record_import(
        self,
        legacy_path: Path,
        row_count: int,
        status: str,
        error: str | None,
    ) -> None:
        try:
            st = legacy_path.stat()
            source_mtime = st.st_mtime
            source_size = st.st_size
        except OSError:
            source_mtime = None
            source_size = None
        self._conn.execute(
            """
            INSERT OR REPLACE INTO legacy_imports(
                source_path, source_mtime, source_size, row_count, status, error
            ) VALUES (?, ?, ?, ?, ?, ?)
            """,
            (str(legacy_path), source_mtime, source_size, row_count, status, error),
        )

    def _upsert_schedule(self, sched: Schedule) -> None:
        now = datetime.now(timezone.utc).isoformat()
        self._conn.execute(
            """
            INSERT OR REPLACE INTO schedules(
                schedule_id, from_peer, to_peer, text, kind, circle, fire_at, cron,
                created_at, updated_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                sched.schedule_id,
                sched.from_peer,
                sched.to_peer,
                sched.text,
                sched.kind,
                sched.circle,
                sched.fire_at,
                sched.cron,
                sched.created_at,
                now,
            ),
        )

    @staticmethod
    def _row_to_schedule(row: sqlite3.Row | None) -> Schedule | None:
        if row is None:
            return None
        return Schedule(
            schedule_id=row["schedule_id"],
            from_peer=row["from_peer"],
            to_peer=row["to_peer"],
            text=row["text"],
            fire_at=row["fire_at"],
            kind=row["kind"],
            circle=row["circle"],
            cron=row["cron"],
            created_at=row["created_at"],
        )

    def persist(self) -> None:
        """Compatibility no-op: SQLite writes commit at mutation time."""
        return

    def create(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        fire_at: datetime,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        cron: str | None = None,
    ) -> Schedule:
        if kind not in _VALID_KINDS:
            raise ValueError(f"kind must be one of {sorted(_VALID_KINDS)}; got {kind!r}")
        if fire_at.tzinfo is None:
            raise ValueError("fire_at must be timezone-aware")
        normalized_cron = validate_cron(cron) if cron is not None else None
        sched = Schedule(
            schedule_id=f"sched-{uuid4().hex[:8]}",
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            fire_at=fire_at.astimezone(timezone.utc).isoformat(),
            kind=kind,
            circle=circle,
            cron=normalized_cron,
        )
        with self._conn:
            self._upsert_schedule(sched)
        return sched

    def create_cron(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        cron: str,
        *,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        now: datetime | None = None,
    ) -> Schedule:
        base = now or datetime.now(timezone.utc)
        normalized_cron = validate_cron(cron)
        return self.create(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            fire_at=next_fire_after(normalized_cron, base),
            kind=kind,
            circle=circle,
            cron=normalized_cron,
        )

    def reschedule_next(self, schedule_id: str, after: datetime | None = None) -> bool:
        sched = self.get(schedule_id)
        if sched is None or sched.cron is None:
            return False
        base = after or datetime.now(timezone.utc)
        sched.fire_at = next_fire_after(sched.cron, base).isoformat()
        with self._conn:
            self._conn.execute(
                "UPDATE schedules SET fire_at = ?, updated_at = ? WHERE schedule_id = ?",
                (sched.fire_at, datetime.now(timezone.utc).isoformat(), schedule_id),
            )
        return True

    def delete(self, schedule_id: str) -> Schedule | None:
        sched = self.get(schedule_id)
        if sched is None:
            return None
        with self._conn:
            self._conn.execute("DELETE FROM schedules WHERE schedule_id = ?", (schedule_id,))
        return sched

    def get(self, schedule_id: str) -> Schedule | None:
        row = self._conn.execute(
            "SELECT * FROM schedules WHERE schedule_id = ?",
            (schedule_id,),
        ).fetchone()
        return self._row_to_schedule(row)

    def list_all(self, from_peer: str | None = None) -> list[Schedule]:
        if from_peer is None:
            rows = self._conn.execute("SELECT * FROM schedules ORDER BY fire_at").fetchall()
        else:
            rows = self._conn.execute(
                "SELECT * FROM schedules WHERE from_peer = ? ORDER BY fire_at",
                (from_peer,),
            ).fetchall()
        return [sched for row in rows if (sched := self._row_to_schedule(row)) is not None]

    def next_due(self) -> Schedule | None:
        row = self._conn.execute(
            "SELECT * FROM schedules ORDER BY fire_at LIMIT 1",
        ).fetchone()
        return self._row_to_schedule(row)

    def export_json_compatible(self) -> dict[str, dict[str, object]]:
        """Return the legacy schedules.json shape for tests/debugging."""
        return {s.schedule_id: asdict(s) for s in self.list_all()}
