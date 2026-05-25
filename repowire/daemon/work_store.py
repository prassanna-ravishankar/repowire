"""Tracked work lifecycle model and store interface."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Literal, Protocol
from uuid import uuid4

WorkState = Literal[
    "queued",
    "dispatching",
    "delivered",
    "running",
    "awaiting_input",
    "completed",
    "failed",
    "cancelled",
    "blocked",
    "expired",
    "unavailable",
]
TerminalWorkState = Literal["completed", "failed", "cancelled", "expired", "unavailable"]

WORK_STATES: frozenset[str] = frozenset(
    {
        "queued",
        "dispatching",
        "delivered",
        "running",
        "awaiting_input",
        "completed",
        "failed",
        "cancelled",
        "blocked",
        "expired",
        "unavailable",
    },
)
TERMINAL_WORK_STATES: frozenset[str] = frozenset(
    {"completed", "failed", "cancelled", "expired", "unavailable"},
)


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def new_work_id() -> str:
    return f"work-{uuid4().hex[:12]}"


def is_terminal_state(state: str) -> bool:
    return state in TERMINAL_WORK_STATES


def validate_state(state: str) -> WorkState:
    if state not in WORK_STATES:
        raise ValueError(f"state must be one of {sorted(WORK_STATES)}; got {state!r}")
    return state  # type: ignore[return-value]


def json_dumps(value: dict[str, Any] | list[Any]) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def json_loads(raw: str | None, default: dict[str, Any] | list[Any]) -> Any:
    if not raw:
        return default.copy() if isinstance(default, dict) else list(default)
    data = json.loads(raw)
    if isinstance(default, dict):
        return data if isinstance(data, dict) else {}
    return data if isinstance(data, list) else []


@dataclass
class TrackedWork:
    """Daemon-owned lifecycle record for durable tracked work."""

    work_id: str
    state: WorkState
    created_at: str
    updated_at: str
    title: str = ""
    kind: str = "general"
    state_reason: str | None = None
    phase: str | None = None
    progress: dict[str, Any] = field(default_factory=dict)
    progress_events: list[Any] = field(default_factory=list)
    owner_peer_id: str | None = None
    assigned_peer_id: str | None = None
    repowire_session_id: str | None = None
    correlation_id: str | None = None
    circle: str | None = None
    created_by_peer_id: str | None = None
    source_kind: str | None = None
    source_id: str | None = None
    scope: str | None = None
    visibility: str = "circle"
    request: dict[str, Any] = field(default_factory=dict)
    deadline_at: str | None = None
    expires_at: str | None = None
    result_summary: str | None = None
    result_data: dict[str, Any] = field(default_factory=dict)
    error: dict[str, Any] = field(default_factory=dict)
    artifacts: list[Any] = field(default_factory=list)
    provenance: dict[str, Any] = field(default_factory=dict)
    cancel_requested: bool = False
    cancel_requested_at: str | None = None
    cancel_requested_by_peer_id: str | None = None
    cancellation_reason: str | None = None
    completed_at: str | None = None

    @property
    def terminal(self) -> bool:
        return is_terminal_state(self.state)

    def status(self) -> dict[str, Any]:
        return {
            "job_id": self.work_id,
            "work_id": self.work_id,
            "title": self.title,
            "kind": self.kind,
            "state": self.state,
            "state_reason": self.state_reason,
            "phase": self.phase,
            "progress": self.progress,
            "progress_events": self.progress_events,
            "owner_peer_id": self.owner_peer_id,
            "assigned_peer_id": self.assigned_peer_id,
            "repowire_session_id": self.repowire_session_id,
            "correlation_id": self.correlation_id,
            "circle": self.circle,
            "created_by_peer_id": self.created_by_peer_id,
            "source_kind": self.source_kind,
            "source_id": self.source_id,
            "scope": self.scope,
            "visibility": self.visibility,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "deadline_at": self.deadline_at,
            "expires_at": self.expires_at,
            "result_summary": self.result_summary,
            "cancel_requested": self.cancel_requested,
            "cancel_requested_at": self.cancel_requested_at,
            "cancel_requested_by_peer_id": self.cancel_requested_by_peer_id,
            "cancellation_reason": self.cancellation_reason,
            "protocol_cancel": self.provenance.get("protocol_cancel"),
            "request": self.request,
            "provenance": self.provenance,
            "execution": self.request.get("execution", {}),
            "runner": self.provenance.get("runner", {}),
            "due_at": ((self.request.get("execution", {}).get("schedule", {}) or {}).get("due_at")),
            "links": self.provenance.get("links", {}),
        }

    def result(self) -> dict[str, Any]:
        if not self.terminal:
            return {
                "job_id": self.work_id,
                "work_id": self.work_id,
                "result_state": "not_ready",
                "status": self.status(),
            }
        return {
            "job_id": self.work_id,
            "work_id": self.work_id,
            "state": self.state,
            "summary": self.result_summary,
            "data": self.result_data,
            "error": self.error,
            "artifacts": self.artifacts,
            "completed_at": self.completed_at,
            "provenance": self.provenance,
        }


class WorkStoreProtocol(Protocol):
    """Persistence adapter interface for tracked work records."""

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
    ) -> TrackedWork: ...

    def get(self, work_id: str) -> TrackedWork | None: ...

    def list_all(
        self,
        *,
        state: str | None = None,
        owner_peer_id: str | None = None,
        created_by_peer_id: str | None = None,
        repowire_session_id: str | None = None,
        circle: str | None = None,
    ) -> list[TrackedWork]: ...

    def acquire_for_dispatch(
        self,
        work_id: str,
        *,
        runner_owner_id: str,
        lease_until: str,
        attempt_id: str | None = None,
        ignore_due_at: bool = False,
        retry: bool = False,
    ) -> TrackedWork | None: ...

    def recover_stale_dispatching(
        self,
        *,
        now: str,
        runner_owner_id: str | None = None,
    ) -> list[TrackedWork]: ...

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
        error: dict[str, Any] | None = None,
    ) -> TrackedWork | None: ...

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
    ) -> TrackedWork | None: ...

    def cancel(
        self,
        work_id: str,
        *,
        requested_by_peer_id: str | None = None,
        reason: str = "cancel_requested",
    ) -> TrackedWork | None: ...
