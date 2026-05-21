"""SQLite-backed session binding metadata store."""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Literal
from uuid import uuid4

from repowire.config.models import AgentType
from repowire.daemon.state.database import StateDatabase

BindingStatus = Literal["active", "detached", "resumable", "archived", "lost", "superseded"]


@dataclass
class SessionBinding:
    """Durable binding between a Repowire workstream and runtime session metadata."""

    repowire_session_id: str
    peer_id: str | None
    current_executor_peer_id: str | None
    backend: str
    project_path: str
    runtime_session_id: str | None = None
    runtime_source_uri: str | None = None
    source_cursor: dict[str, Any] = field(default_factory=dict)
    provenance: dict[str, Any] = field(default_factory=dict)
    resume_capability: dict[str, Any] = field(default_factory=dict)
    status: BindingStatus = "active"
    metadata: dict[str, Any] = field(default_factory=dict)
    created_at: str = ""
    last_seen_at: str = ""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _json_dumps(value: dict[str, Any]) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def _json_loads(raw: str | None) -> dict[str, Any]:
    if not raw:
        return {}
    data = json.loads(raw)
    return data if isinstance(data, dict) else {}


def _backend_value(backend: AgentType | str) -> str:
    return backend.value if isinstance(backend, AgentType) else str(backend)


def _merge_dicts(existing: dict[str, Any], updates: dict[str, Any] | None) -> dict[str, Any]:
    if not updates:
        return dict(existing)
    merged = dict(existing)
    merged.update({k: v for k, v in updates.items() if v is not None})
    return merged


class SQLiteSessionBindingStore:
    """Repository for runtime session binding/provenance metadata.

    The store intentionally persists only identifiers, source locators, cursors,
    and small metadata envelopes. Raw runtime transcript bodies stay in the
    backend-owned source files/databases.
    """

    def __init__(self, db: StateDatabase) -> None:
        self._db = db
        self._conn = db.conn

    @staticmethod
    def _row_to_binding(row: sqlite3.Row | None) -> SessionBinding | None:
        if row is None:
            return None
        return SessionBinding(
            repowire_session_id=row["repowire_session_id"],
            peer_id=row["peer_id"],
            current_executor_peer_id=row["current_executor_peer_id"],
            backend=row["backend"],
            project_path=row["project_path"],
            runtime_session_id=row["runtime_session_id"],
            runtime_source_uri=row["runtime_source_uri"],
            source_cursor=_json_loads(row["source_cursor"]),
            provenance=_json_loads(row["provenance"]),
            resume_capability=_json_loads(row["resume_capability"]),
            status=row["status"],
            metadata=_json_loads(row["metadata"]),
            created_at=row["created_at"],
            last_seen_at=row["last_seen_at"],
        )

    def _find_existing(
        self,
        *,
        peer_id: str | None,
        backend: str,
        project_path: str,
        runtime_session_id: str | None,
        runtime_source_uri: str | None,
    ) -> SessionBinding | None:
        if runtime_source_uri:
            found = self.get_by_source_uri(runtime_source_uri)
            if found is not None:
                return found
        if runtime_session_id:
            found = self.get_by_runtime_session(
                runtime_session_id,
                backend=backend,
                project_path=project_path or None,
            )
            if found is not None:
                return found
        if peer_id:
            rows = self._conn.execute(
                """
                SELECT * FROM session_bindings
                WHERE peer_id = ? AND backend = ? AND project_path = ?
                  AND runtime_session_id IS NULL AND runtime_source_uri IS NULL
                ORDER BY last_seen_at DESC
                LIMIT 1
                """,
                (peer_id, backend, project_path),
            ).fetchone()
            return self._row_to_binding(rows)
        return None

    def upsert_observation(
        self,
        *,
        peer_id: str | None,
        backend: AgentType | str,
        project_path: str | None,
        runtime_session_id: str | None = None,
        runtime_source_uri: str | None = None,
        source_cursor: dict[str, Any] | None = None,
        provenance: dict[str, Any] | None = None,
        resume_capability: dict[str, Any] | None = None,
        status: BindingStatus = "active",
        metadata: dict[str, Any] | None = None,
        observed_at: str | None = None,
    ) -> SessionBinding:
        """Create or update the binding matching an observed runtime edge."""
        normalized_backend = _backend_value(backend)
        normalized_path = project_path or ""
        now = observed_at or _now()
        existing = self._find_existing(
            peer_id=peer_id,
            backend=normalized_backend,
            project_path=normalized_path,
            runtime_session_id=runtime_session_id,
            runtime_source_uri=runtime_source_uri,
        )

        if existing is None:
            binding = SessionBinding(
                repowire_session_id=f"rw-session-{uuid4().hex[:12]}",
                peer_id=peer_id,
                current_executor_peer_id=peer_id,
                backend=normalized_backend,
                project_path=normalized_path,
                runtime_session_id=runtime_session_id,
                runtime_source_uri=runtime_source_uri,
                source_cursor=source_cursor or {},
                provenance=provenance or {},
                resume_capability=resume_capability or {},
                status=status,
                metadata=metadata or {},
                created_at=now,
                last_seen_at=now,
            )
        else:
            binding = SessionBinding(
                repowire_session_id=existing.repowire_session_id,
                peer_id=peer_id or existing.peer_id,
                current_executor_peer_id=peer_id or existing.current_executor_peer_id,
                backend=normalized_backend,
                project_path=normalized_path or existing.project_path,
                runtime_session_id=runtime_session_id or existing.runtime_session_id,
                runtime_source_uri=runtime_source_uri or existing.runtime_source_uri,
                source_cursor=_merge_dicts(existing.source_cursor, source_cursor),
                provenance=_merge_dicts(existing.provenance, provenance),
                resume_capability=_merge_dicts(existing.resume_capability, resume_capability),
                status=status or existing.status,
                metadata=_merge_dicts(existing.metadata, metadata),
                created_at=existing.created_at,
                last_seen_at=now,
            )

        with self._conn:
            self._conn.execute(
                """
                INSERT OR REPLACE INTO session_bindings(
                    repowire_session_id, peer_id, current_executor_peer_id, backend,
                    project_path, runtime_session_id, runtime_source_uri, source_cursor,
                    provenance, resume_capability, status, metadata, created_at, last_seen_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    binding.repowire_session_id,
                    binding.peer_id,
                    binding.current_executor_peer_id,
                    binding.backend,
                    binding.project_path,
                    binding.runtime_session_id,
                    binding.runtime_source_uri,
                    _json_dumps(binding.source_cursor),
                    _json_dumps(binding.provenance),
                    _json_dumps(binding.resume_capability),
                    binding.status,
                    _json_dumps(binding.metadata),
                    binding.created_at,
                    binding.last_seen_at,
                ),
            )
        return binding

    def get(self, repowire_session_id: str) -> SessionBinding | None:
        row = self._conn.execute(
            "SELECT * FROM session_bindings WHERE repowire_session_id = ?",
            (repowire_session_id,),
        ).fetchone()
        return self._row_to_binding(row)

    def list_by_peer(self, peer_id: str) -> list[SessionBinding]:
        rows = self._conn.execute(
            "SELECT * FROM session_bindings WHERE peer_id = ? ORDER BY last_seen_at DESC",
            (peer_id,),
        ).fetchall()
        return [binding for row in rows if (binding := self._row_to_binding(row)) is not None]

    def get_by_runtime_session(
        self,
        runtime_session_id: str,
        *,
        backend: AgentType | str | None = None,
        project_path: str | None = None,
    ) -> SessionBinding | None:
        clauses = ["runtime_session_id = ?"]
        params: list[str] = [runtime_session_id]
        if backend is not None:
            clauses.append("backend = ?")
            params.append(_backend_value(backend))
        if project_path is not None:
            clauses.append("project_path = ?")
            params.append(project_path)
        row = self._conn.execute(
            f"""
            SELECT * FROM session_bindings
            WHERE {' AND '.join(clauses)}
            ORDER BY last_seen_at DESC
            LIMIT 1
            """,
            params,
        ).fetchone()
        return self._row_to_binding(row)

    def list_by_backend_project(
        self,
        *,
        backend: AgentType | str,
        project_path: str,
    ) -> list[SessionBinding]:
        rows = self._conn.execute(
            """
            SELECT * FROM session_bindings
            WHERE backend = ? AND project_path = ?
            ORDER BY last_seen_at DESC
            """,
            (_backend_value(backend), project_path),
        ).fetchall()
        return [binding for row in rows if (binding := self._row_to_binding(row)) is not None]

    def get_by_source_uri(self, runtime_source_uri: str) -> SessionBinding | None:
        row = self._conn.execute(
            """
            SELECT * FROM session_bindings
            WHERE runtime_source_uri = ?
            ORDER BY last_seen_at DESC
            LIMIT 1
            """,
            (runtime_source_uri,),
        ).fetchone()
        return self._row_to_binding(row)

    def list_all(self) -> list[SessionBinding]:
        rows = self._conn.execute(
            "SELECT * FROM session_bindings ORDER BY last_seen_at DESC",
        ).fetchall()
        return [binding for row in rows if (binding := self._row_to_binding(row)) is not None]


def resolve_repowire_session_id(
    store: SQLiteSessionBindingStore | None,
    *,
    peer: Any | None = None,
    peer_id: str | None = None,
    backend: AgentType | str | None = None,
    project_path: str | None = None,
    runtime_session_id: str | None = None,
) -> str | None:
    """Best-effort existing binding lookup for control-event annotations.

    This intentionally never creates a binding. Control events should carry a
    session pointer only when the daemon can resolve an already-observed
    binding without changing routing or lifecycle state.
    """
    if store is None:
        return None

    if peer is not None:
        peer_id = peer_id or getattr(peer, "peer_id", None)
        backend = backend or getattr(peer, "backend", None)
        project_path = project_path or getattr(peer, "path", None)
        metadata = getattr(peer, "metadata", None)
        if runtime_session_id is None and isinstance(metadata, dict):
            value = metadata.get("hook_session_id") or metadata.get("session_id")
            if isinstance(value, str) and value:
                runtime_session_id = value

    if runtime_session_id:
        binding = store.get_by_runtime_session(
            runtime_session_id,
            backend=backend,
            project_path=project_path,
        )
        if binding is not None:
            return binding.repowire_session_id

    if peer_id:
        bindings = store.list_by_peer(peer_id)
        if bindings:
            return bindings[0].repowire_session_id

    if backend is not None and project_path:
        bindings = store.list_by_backend_project(
            backend=backend,
            project_path=project_path,
        )
        if len(bindings) == 1:
            return bindings[0].repowire_session_id

    return None
