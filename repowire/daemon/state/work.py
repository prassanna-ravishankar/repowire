"""SQLite-backed tracked work persistence adapter."""

from __future__ import annotations

import logging
import sqlite3
from collections.abc import Callable
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

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


def _runner_provenance(work: TrackedWork) -> dict[str, Any]:
    provenance = dict(work.provenance)
    runner = dict(provenance.get("runner") or {})
    runner.setdefault("attempt_count", len(runner.get("attempts") or []))
    runner.setdefault("attempts", [])
    provenance["runner"] = runner
    return provenance


def _runner_current_attempt(work: TrackedWork) -> str | None:
    runner = (work.provenance or {}).get("runner") or {}
    current = runner.get("current_attempt_id")
    return current if isinstance(current, str) and current else None


def _parse_iso(value: str | None) -> datetime | None:
    if not value:
        return None
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


logger = logging.getLogger(__name__)

#: Fired after a committed state transition: (work_after, prior_state).
StateChangeCallback = Callable[[TrackedWork, str | None], None]


class SQLiteWorkStore:
    """Tracked work store backed by daemon state DB."""

    def __init__(
        self, db: StateDatabase, *, on_state_change: StateChangeCallback | None = None,
    ) -> None:
        self._db = db
        self._conn = db.conn
        self._on_state_change = on_state_change

    def set_on_state_change(self, callback: StateChangeCallback | None) -> None:
        """Observe committed state transitions (events, terminal side effects)."""
        self._on_state_change = callback

    def _emit_state_change(self, work: TrackedWork | None, prior_state: str | None) -> None:
        if work is None or self._on_state_change is None or work.state == prior_state:
            return
        try:
            self._on_state_change(work, prior_state)
        except Exception:
            logger.exception("work state-change callback failed for %s", work.work_id)

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

    def _replace_work_json(
        self,
        work_id: str,
        *,
        state: str,
        state_reason: str | None,
        phase: str | None,
        assigned_peer_id: str | None,
        correlation_id: str | None,
        provenance: dict[str, Any],
        error: dict[str, Any] | None = None,
        completed_at: str | None = None,
    ) -> None:
        now = now_iso()
        self._conn.execute(
            """
            UPDATE tracked_work SET
                state = ?, state_reason = ?, phase = ?, assigned_peer_id = ?,
                correlation_id = ?, provenance_json = ?, error_json = ?,
                completed_at = ?, updated_at = ?
            WHERE work_id = ?
            """,
            (
                state,
                state_reason,
                phase,
                assigned_peer_id,
                correlation_id,
                json_dumps(provenance),
                json_dumps(error or {}),
                completed_at,
                now,
                work_id,
            ),
        )

    def acquire_for_dispatch(
        self,
        work_id: str,
        *,
        runner_owner_id: str,
        lease_until: str,
        attempt_id: str | None = None,
        ignore_due_at: bool = False,
        retry: bool = False,
    ) -> TrackedWork | None:
        now = now_iso()
        attempt_id = attempt_id or f"attempt-{uuid4().hex[:12]}"
        with self._conn:
            existing = self.get(work_id)
            if existing is None or existing.cancel_requested:
                return None
            allowed = (
                {"queued", "failed", "unavailable"}
                if not retry
                else {"failed", "unavailable", "delivered"}
            )
            if existing.state not in allowed:
                return None
            if retry and existing.state not in {"failed", "unavailable", "delivered"}:
                return None
            due_at = (((existing.request or {}).get("execution") or {}).get("schedule") or {}).get(
                "due_at"
            )
            due = _parse_iso(due_at)
            if due and not ignore_due_at and due > datetime.now(timezone.utc):
                return None
            provenance = _runner_provenance(existing)
            runner = provenance["runner"]
            attempts = list(runner.get("attempts") or [])
            attempt = {
                "attempt_id": attempt_id,
                "status": "dispatching",
                "phase": "acquired",
                "started_at": now,
                "completed_at": None,
                "runner_owner_id": runner_owner_id,
                "lease_until": lease_until,
                "assigned_peer_id": existing.assigned_peer_id,
                "assigned_peer_info": {},
                "tmux": {},
                "correlation_id": None,
                "delivery_state": None,
                "error": {},
            }
            attempts.append(attempt)
            runner.update(
                {
                    "attempt_count": len(attempts),
                    "current_attempt_id": attempt_id,
                    "runner_owner_id": runner_owner_id,
                    "acquired_at": now,
                    "lease_until": lease_until,
                    "attempts": attempts,
                }
            )
            provenance["runner"] = runner
            self._replace_work_json(
                work_id,
                state="dispatching",
                state_reason="dispatching",
                phase="acquired",
                assigned_peer_id=existing.assigned_peer_id,
                correlation_id=existing.correlation_id,
                provenance=provenance,
            )
        acquired = self.get(work_id)
        self._emit_state_change(acquired, existing.state)
        return acquired

    def recover_stale_dispatching(
        self,
        *,
        now: str,
        runner_owner_id: str | None = None,
    ) -> list[TrackedWork]:
        """Recover work stuck mid-dispatch after a runner interruption.

        Only ``dispatching`` is lease-bounded: it is the runner's own phase, so
        an expired lease there means the runner died mid-acquisition. Once a
        job is ``delivered`` the executor owns the fire — its liveness is
        tracked by the fire-completion service (executor terminal offline),
        never by a wall-clock lease.
        """
        recovered: list[TrackedWork] = []
        rows = self._conn.execute(
            "SELECT * FROM tracked_work WHERE state = 'dispatching'"
        ).fetchall()
        with self._conn:
            for row in rows:
                work = self._row_to_work(row)
                if work is None:
                    continue
                provenance = _runner_provenance(work)
                runner = provenance["runner"]
                lease_until = runner.get("lease_until")
                lease = _parse_iso(lease_until)
                current_time = _parse_iso(now) or datetime.now(timezone.utc)
                if lease and lease > current_time:
                    continue
                attempt_id = runner.get("current_attempt_id")
                attempts = list(runner.get("attempts") or [])
                for attempt in attempts:
                    if attempt.get("attempt_id") == attempt_id:
                        attempt.update(
                            {
                                "status": "unavailable",
                                "phase": "runner_interrupted",
                                "completed_at": now,
                                "error": {"reason": "runner_interrupted"},
                            }
                        )
                        break
                runner.update(
                    {
                        "attempts": attempts,
                        "last_error": {"reason": "runner_interrupted"},
                        "current_attempt_id": attempt_id,
                    }
                )
                provenance["runner"] = runner
                self._replace_work_json(
                    work.work_id,
                    state="unavailable",
                    state_reason="runner_interrupted",
                    phase="runner_interrupted",
                    assigned_peer_id=work.assigned_peer_id,
                    correlation_id=work.correlation_id,
                    provenance=provenance,
                    error={"reason": "runner_interrupted"},
                    completed_at=now,
                )
                refreshed = self.get(work.work_id)
                if refreshed is not None:
                    recovered.append(refreshed)
                    self._emit_state_change(refreshed, work.state)
        return recovered

    def update_attempt(
        self,
        work_id: str,
        *,
        attempt_id: str,
        status: str | None = None,
        phase: str | None = None,
        assigned_peer_id: str | None = None,
        assigned_peer_info: dict[str, Any] | None = None,
        tmux: dict[str, Any] | None = None,
        correlation_id: str | None = None,
        delivery_state: str | None = None,
        resume_plan: dict[str, Any] | None = None,
        operation_id: str | None = None,
        acquisition_strategy: str | None = None,
        acquisition: dict[str, Any] | None = None,
        error: dict[str, Any] | None = None,
    ) -> TrackedWork | None:
        with self._conn:
            existing = self.get(work_id)
            if existing is None:
                return None
            provenance = _runner_provenance(existing)
            runner = provenance["runner"]
            attempts = list(runner.get("attempts") or [])
            found = False
            completed_at = existing.completed_at
            for attempt in attempts:
                if attempt.get("attempt_id") != attempt_id:
                    continue
                found = True
                if status is not None:
                    attempt["status"] = status
                if phase is not None:
                    attempt["phase"] = phase
                if assigned_peer_id is not None:
                    attempt["assigned_peer_id"] = assigned_peer_id
                if assigned_peer_info is not None:
                    attempt["assigned_peer_info"] = assigned_peer_info
                if tmux is not None:
                    attempt["tmux"] = tmux
                if correlation_id is not None:
                    attempt["correlation_id"] = correlation_id
                if delivery_state is not None:
                    attempt["delivery_state"] = delivery_state
                if resume_plan is not None:
                    attempt["resume_plan"] = resume_plan
                if operation_id is not None:
                    attempt["operation_id"] = operation_id
                if acquisition_strategy is not None:
                    attempt["acquisition_strategy"] = acquisition_strategy
                if acquisition is not None:
                    attempt["acquisition"] = acquisition
                if error is not None:
                    attempt["error"] = error
                if status in {
                    "delivered", "completed", "failed",
                    "unavailable", "interrupted", "cancelled",
                }:
                    attempt["completed_at"] = now_iso()
                break
            if not found:
                return None
            runner["attempts"] = attempts
            if error:
                runner["last_error"] = error
            provenance["runner"] = runner
            is_current = runner.get("current_attempt_id") == attempt_id
            if not is_current:
                self._conn.execute(
                    """
                    UPDATE tracked_work SET provenance_json = ?, updated_at = ?
                    WHERE work_id = ?
                    """,
                    (json_dumps(provenance), now_iso(), work_id),
                )
                return self.get(work_id)
            state = existing.state
            reason = existing.state_reason
            new_phase = phase if phase is not None else existing.phase
            if status == "delivered":
                state, reason = "delivered", "ask_delivered"
            elif status == "failed":
                state, reason = "failed", (error or {}).get("reason", "dispatch_failed")
                completed_at = completed_at or now_iso()
            elif status == "unavailable":
                state, reason = "unavailable", (error or {}).get("reason", "unavailable")
                completed_at = completed_at or now_iso()
            elif status == "cancelled":
                state, reason = "cancelled", "cancel_requested"
                completed_at = completed_at or now_iso()
            self._replace_work_json(
                work_id,
                state=state,
                state_reason=reason,
                phase=new_phase,
                assigned_peer_id=assigned_peer_id
                if assigned_peer_id is not None
                else existing.assigned_peer_id,
                correlation_id=correlation_id
                if correlation_id is not None
                else existing.correlation_id,
                provenance=provenance,
                error=error if error is not None else existing.error,
                completed_at=completed_at,
            )
        updated = self.get(work_id)
        self._emit_state_change(updated, existing.state)
        return updated

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
        attempt_id: str | None = None,
    ) -> TrackedWork | None:
        validate_state(state)
        existing = self.get(work_id)
        if existing is None:
            return None
        current_attempt_id = _runner_current_attempt(existing)
        if current_attempt_id and state in {"completed", "failed", "cancelled"}:
            if not attempt_id:
                raise ValueError("attempt_id is required for runner-managed job updates")
            if attempt_id != current_attempt_id:
                raise RuntimeError("stale_attempt")
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
        updated = self.get(work_id)
        self._emit_state_change(updated, existing.state)
        return updated

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
            cancelled = self.get(work_id)
            self._emit_state_change(cancelled, existing.state)
            return cancelled
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
