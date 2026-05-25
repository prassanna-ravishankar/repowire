"""SQLite-backed tracked work persistence adapter."""

from __future__ import annotations

import sqlite3
from datetime import datetime, timezone
from typing import Any

from repowire.daemon.state.database import StateDatabase
from repowire.daemon.work_store import (
    TrackedWork,
    WorkState,
    is_terminal_state,
    json_dumps,
    json_loads,
    new_work_id,
    now_iso,
    validate_state,
)


class SQLiteWorkStore:
    """Tracked work store backed by daemon state DB."""

    def __init__(self, db: StateDatabase) -> None:
        self._db = db
        self._conn = db.conn

    @staticmethod
    def _row_to_work(row: sqlite3.Row | None) -> TrackedWork | None:
        if row is None:
            return None
        return TrackedWork(
            work_id=row["work_id"],
            title=row["title"],
            kind=row["kind"],
            state=validate_state(row["state"]),
            state_reason=row["state_reason"],
            phase=row["phase"],
            progress=json_loads(row["progress_json"], {}),
            progress_events=json_loads(row["progress_events_json"], []),
            owner_peer_id=row["owner_peer_id"],
            assigned_peer_id=row["assigned_peer_id"],
            repowire_session_id=row["repowire_session_id"],
            correlation_id=row["correlation_id"],
            circle=row["circle"],
            created_by_peer_id=row["created_by_peer_id"],
            source_kind=row["source_kind"],
            source_id=row["source_id"],
            scope=row["scope"],
            visibility=row["visibility"],
            request=json_loads(row["request_json"], {}),
            deadline_at=row["deadline_at"],
            expires_at=row["expires_at"],
            result_summary=row["result_summary"],
            result_data=json_loads(row["result_data_json"], {}),
            error=json_loads(row["error_json"], {}),
            artifacts=json_loads(row["artifacts_json"], []),
            provenance=json_loads(row["provenance_json"], {}),
            cancel_requested=bool(row["cancel_requested"]),
            cancel_requested_at=row["cancel_requested_at"],
            cancel_requested_by_peer_id=row["cancel_requested_by_peer_id"],
            cancellation_reason=row["cancellation_reason"],
            completed_at=row["completed_at"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )

    def create(
        self,
        *,
        title: str = "",
        kind: str = "general",
        created_by_peer_id: str | None = None,
        owner_peer_id: str | None = None,
        assigned_peer_id: str | None = None,
        repowire_session_id: str | None = None,
        correlation_id: str | None = None,
        circle: str | None = None,
        source_kind: str | None = None,
        source_id: str | None = None,
        scope: str | None = None,
        visibility: str = "circle",
        request: dict[str, Any] | None = None,
        deadline_at: str | None = None,
        expires_at: str | None = None,
        provenance: dict[str, Any] | None = None,
    ) -> TrackedWork:
        now = now_iso()
        work = TrackedWork(
            work_id=new_work_id(),
            title=title,
            kind=kind,
            state="queued",
            state_reason=None,
            phase=None,
            progress={},
            progress_events=[],
            owner_peer_id=owner_peer_id,
            assigned_peer_id=assigned_peer_id,
            repowire_session_id=repowire_session_id,
            correlation_id=correlation_id,
            circle=circle,
            created_by_peer_id=created_by_peer_id,
            source_kind=source_kind,
            source_id=source_id,
            scope=scope,
            visibility=visibility,
            request=request or {},
            deadline_at=deadline_at,
            expires_at=expires_at,
            provenance=provenance or {},
            created_at=now,
            updated_at=now,
        )
        with self._conn:
            self._conn.execute(
                """
                INSERT INTO tracked_work(
                    work_id, title, kind, state, state_reason, phase, progress_json,
                    progress_events_json, owner_peer_id, assigned_peer_id,
                    repowire_session_id, correlation_id, circle, created_by_peer_id,
                    source_kind, source_id, scope, visibility, request_json,
                    deadline_at, expires_at, result_summary, result_data_json,
                    error_json, artifacts_json, provenance_json, cancel_requested,
                    cancel_requested_at, cancel_requested_by_peer_id,
                    cancellation_reason, completed_at,
                    created_at, updated_at
                ) VALUES (
                    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
                )
                """,
                (
                    work.work_id,
                    work.title,
                    work.kind,
                    work.state,
                    work.state_reason,
                    work.phase,
                    json_dumps(work.progress),
                    json_dumps(work.progress_events),
                    work.owner_peer_id,
                    work.assigned_peer_id,
                    work.repowire_session_id,
                    work.correlation_id,
                    work.circle,
                    work.created_by_peer_id,
                    work.source_kind,
                    work.source_id,
                    work.scope,
                    work.visibility,
                    json_dumps(work.request),
                    work.deadline_at,
                    work.expires_at,
                    work.result_summary,
                    json_dumps(work.result_data),
                    json_dumps(work.error),
                    json_dumps(work.artifacts),
                    json_dumps(work.provenance),
                    int(work.cancel_requested),
                    work.cancel_requested_at,
                    work.cancel_requested_by_peer_id,
                    work.cancellation_reason,
                    work.completed_at,
                    work.created_at,
                    work.updated_at,
                ),
            )
        return work

    def get(self, work_id: str) -> TrackedWork | None:
        row = self._conn.execute(
            "SELECT * FROM tracked_work WHERE work_id = ?",
            (work_id,),
        ).fetchone()
        return self._row_to_work(row)

    def list_all(
        self,
        *,
        state: str | None = None,
        owner_peer_id: str | None = None,
        created_by_peer_id: str | None = None,
        repowire_session_id: str | None = None,
        circle: str | None = None,
    ) -> list[TrackedWork]:
        clauses: list[str] = []
        params: list[str] = []
        if state is not None:
            validate_state(state)
            clauses.append("state = ?")
            params.append(state)
        if owner_peer_id is not None:
            clauses.append("owner_peer_id = ?")
            params.append(owner_peer_id)
        if created_by_peer_id is not None:
            clauses.append("created_by_peer_id = ?")
            params.append(created_by_peer_id)
        if repowire_session_id is not None:
            clauses.append("repowire_session_id = ?")
            params.append(repowire_session_id)
        if circle is not None:
            clauses.append("circle = ?")
            params.append(circle)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        rows = self._conn.execute(
            f"SELECT * FROM tracked_work {where} ORDER BY updated_at DESC",
            params,
        ).fetchall()
        return [work for row in rows if (work := self._row_to_work(row)) is not None]

    def update_state(
        self,
        work_id: str,
        *,
        state: WorkState,
        state_reason: str | None = None,
        phase: str | None = None,
        progress: dict[str, Any] | None = None,
        progress_note: str | None = None,
        result_summary: str | None = None,
        result_data: dict[str, Any] | None = None,
        error: dict[str, Any] | None = None,
        artifacts: list[Any] | None = None,
        provenance: dict[str, Any] | None = None,
    ) -> TrackedWork | None:
        validate_state(state)
        existing = self.get(work_id)
        if existing is None:
            return None
        if existing.terminal and state != existing.state:
            raise ValueError(
                f"terminal work {work_id} is already {existing.state}; "
                "terminal state cannot be changed",
            )
        completed_at = existing.completed_at
        if is_terminal_state(state) and completed_at is None:
            completed_at = datetime.now(timezone.utc).isoformat()
        progress_events = list(existing.progress_events)
        if progress_note:
            progress_events.append(
                {
                    "at": now_iso(),
                    "note": progress_note,
                    "state": state,
                    "phase": phase if phase is not None else existing.phase,
                },
            )
        with self._conn:
            self._conn.execute(
                """
                UPDATE tracked_work SET
                    state = ?,
                    state_reason = ?,
                    phase = ?,
                    progress_json = ?,
                    progress_events_json = ?,
                    result_summary = ?,
                    result_data_json = ?,
                    error_json = ?,
                    artifacts_json = ?,
                    provenance_json = ?,
                    completed_at = ?,
                    updated_at = ?
                WHERE work_id = ?
                """,
                (
                    state,
                    state_reason,
                    phase,
                    json_dumps(progress if progress is not None else existing.progress),
                    json_dumps(progress_events),
                    result_summary if result_summary is not None else existing.result_summary,
                    json_dumps(result_data if result_data is not None else existing.result_data),
                    json_dumps(error if error is not None else existing.error),
                    json_dumps(artifacts if artifacts is not None else existing.artifacts),
                    json_dumps(provenance if provenance is not None else existing.provenance),
                    completed_at,
                    now_iso(),
                    work_id,
                ),
            )
        return self.get(work_id)

    def cancel(
        self,
        work_id: str,
        *,
        requested_by_peer_id: str | None = None,
        reason: str = "cancel_requested",
    ) -> TrackedWork | None:
        existing = self.get(work_id)
        if existing is None:
            return None
        requested_at = now_iso()
        if existing.terminal:
            with self._conn:
                self._conn.execute(
                    """
                    UPDATE tracked_work SET
                        cancel_requested = 1,
                        cancel_requested_at = COALESCE(cancel_requested_at, ?),
                        cancel_requested_by_peer_id = COALESCE(cancel_requested_by_peer_id, ?),
                        cancellation_reason = COALESCE(cancellation_reason, ?),
                        updated_at = ?
                    WHERE work_id = ?
                    """,
                    (requested_at, requested_by_peer_id, reason, requested_at, work_id),
                )
            return self.get(work_id)
        if existing.state == "queued":
            with self._conn:
                self._conn.execute(
                    """
                    UPDATE tracked_work SET
                        state = 'cancelled',
                        state_reason = ?,
                        cancel_requested = 1,
                        cancel_requested_at = ?,
                        cancel_requested_by_peer_id = ?,
                        cancellation_reason = ?,
                        completed_at = ?,
                        updated_at = ?
                    WHERE work_id = ?
                    """,
                    (
                        reason,
                        requested_at,
                        requested_by_peer_id,
                        reason,
                        requested_at,
                        requested_at,
                        work_id,
                    ),
                )
            return self.get(work_id)
        with self._conn:
            self._conn.execute(
                """
                UPDATE tracked_work SET
                    state_reason = ?,
                    cancel_requested = 1,
                    cancel_requested_at = ?,
                    cancel_requested_by_peer_id = ?,
                    cancellation_reason = ?,
                    updated_at = ?
                WHERE work_id = ?
                """,
                (reason, requested_at, requested_by_peer_id, reason, requested_at, work_id),
            )
        return self.get(work_id)
