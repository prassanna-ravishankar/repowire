"""Human approval broker for ACP permission requests."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal
from uuid import uuid4

PermissionOutcome = Literal["allowed", "denied", "cancelled"]


@dataclass(frozen=True)
class PermissionDecision:
    """Resolved ACP permission decision."""

    outcome: PermissionOutcome
    option_id: str | None = None
    message: str | None = None
    timed_out: bool = False


@dataclass
class _PendingPermission:
    request_id: str
    peer_id: str
    session_id: str
    tool_call: dict[str, Any]
    options: list[dict[str, Any]]
    future: asyncio.Future[PermissionDecision] = field(repr=False)


class ApprovalBroker:
    """Small in-process broker for ACP tool permission decisions.

    The broker emits dashboard event-log records for visibility, waits for a
    human decision submitted through the daemon, and defaults to deny when the
    decision does not arrive before the timeout.
    """

    def __init__(
        self,
        *,
        emit_event: Callable[[str, dict[str, Any]], str],
        resolve_repowire_session_id: Callable[[str, str], str | None] | None = None,
        timeout_seconds: float = 60.0,
    ) -> None:
        self._emit_event = emit_event
        self._resolve_repowire_session_id = resolve_repowire_session_id
        self._timeout_seconds = timeout_seconds
        self._pending: dict[str, _PendingPermission] = {}
        self._lock = asyncio.Lock()
        self._last_error: str | None = None
        self._last_error_at: str | None = None

    async def request_permission(
        self,
        *,
        peer_id: str,
        session_id: str,
        tool_call: Any,
        options: list[Any],
    ) -> PermissionDecision:
        """Emit a permission request and wait for a decision.

        Timeout intentionally defaults to deny. This is safer than preserving
        the prior auto-allow fallback, and keeps the ACP subprocess unblocked.
        """
        loop = asyncio.get_running_loop()
        normalized_options = [_normalize_option(option) for option in options]
        pending = _PendingPermission(
            request_id=f"acpperm-{uuid4().hex[:12]}",
            peer_id=peer_id,
            session_id=session_id,
            tool_call=_normalize_payload(tool_call),
            options=normalized_options,
            future=loop.create_future(),
        )
        async with self._lock:
            self._pending[pending.request_id] = pending

        repowire_session_id = self._resolve_session_id(peer_id, session_id)
        self._emit_event(
            "acp_permission_request",
            {
                "request_id": pending.request_id,
                "peer_id": peer_id,
                "session_id": session_id,
                "repowire_session_id": repowire_session_id,
                "tool_call": pending.tool_call,
                "options": normalized_options,
                "status": "pending",
                "timeout_seconds": self._timeout_seconds,
            },
        )

        try:
            decision = await asyncio.wait_for(pending.future, timeout=self._timeout_seconds)
        except asyncio.TimeoutError:
            decision = PermissionDecision(
                outcome="denied",
                message="permission request timed out",
                timed_out=True,
            )
            self._last_error = "permission request timed out"
            self._last_error_at = _utc_now()
        finally:
            async with self._lock:
                self._pending.pop(pending.request_id, None)

        self._emit_decision_event(pending, decision)
        return decision

    async def decide(
        self,
        request_id: str,
        *,
        outcome: PermissionOutcome,
        option_id: str | None = None,
        message: str | None = None,
    ) -> PermissionDecision | None:
        """Resolve a pending request.

        Returns ``None`` when the request is unknown or already resolved.
        """
        async with self._lock:
            pending = self._pending.get(request_id)
            if pending is None or pending.future.done():
                return None
            if outcome == "allowed":
                option_id = option_id or _first_option_id(pending.options)
            decision = PermissionDecision(
                outcome=outcome,
                option_id=option_id,
                message=message,
            )
            pending.future.set_result(decision)
            self._last_error = None
            self._last_error_at = None
            return decision

    async def get_pending(self, request_id: str) -> dict[str, Any] | None:
        """Return a pending request snapshot for validation/UI callers."""
        async with self._lock:
            pending = self._pending.get(request_id)
            if pending is None:
                return None
            return {
                "request_id": pending.request_id,
                "peer_id": pending.peer_id,
                "session_id": pending.session_id,
                "tool_call": pending.tool_call,
                "options": list(pending.options),
            }

    def _emit_decision_event(
        self,
        pending: _PendingPermission,
        decision: PermissionDecision,
    ) -> None:
        status = "timed_out" if decision.timed_out else "decided"
        repowire_session_id = self._resolve_session_id(pending.peer_id, pending.session_id)
        self._emit_event(
            "acp_permission_decision",
            {
                "request_id": pending.request_id,
                "peer_id": pending.peer_id,
                "session_id": pending.session_id,
                "repowire_session_id": repowire_session_id,
                "outcome": decision.outcome,
                "option_id": decision.option_id,
                "message": decision.message,
                "status": status,
            },
        )

    def _resolve_session_id(self, peer_id: str, session_id: str) -> str | None:
        if self._resolve_repowire_session_id is None:
            return None
        try:
            return self._resolve_repowire_session_id(peer_id, session_id)
        except Exception:
            return None

    def health_snapshot(self) -> dict[str, Any]:
        """Return passive permission relay state for /health and doctor."""
        return {
            "pending": len(self._pending),
            "timeout_seconds": self._timeout_seconds,
            "last_error": self._last_error,
            "last_error_at": self._last_error_at,
        }


def _first_option_id(options: list[dict[str, Any]]) -> str | None:
    for option in options:
        option_id = option.get("option_id")
        if isinstance(option_id, str) and option_id:
            return option_id
    return None


def _normalize_option(option: Any) -> dict[str, Any]:
    payload = _normalize_payload(option)
    option_id = payload.get("option_id") or getattr(option, "option_id", None)
    if option_id is not None:
        payload["option_id"] = str(option_id)
    return payload


def _normalize_payload(value: Any) -> dict[str, Any]:
    """Best-effort compact serialization for ACP SDK models."""
    if value is None:
        return {}
    if isinstance(value, dict):
        return dict(value)
    if hasattr(value, "model_dump"):
        dumped = value.model_dump(mode="json", exclude_none=True)
        if isinstance(dumped, dict):
            return dumped
    out: dict[str, Any] = {}
    for name in ("tool_call_id", "name", "title", "kind", "option_id", "description"):
        attr = getattr(value, name, None)
        if attr is not None:
            out[name] = attr
    return out or {"repr": repr(value)}


def _utc_now() -> str:
    return datetime.now(UTC).isoformat()
