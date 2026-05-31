"""ACP tool-permission adapter over the shared mesh-questions primitive.

This is no longer a parallel decision system: a ``RequestPermission`` from an
ACP runtime is translated into a blocking *choice question* ask registered in
the ``AskTracker`` (recipient = the virtual ``__repowire_control__`` surface, so
human answer-paths — dashboard banner, Telegram buttons — render it like any
other question, and a peer can answer it programmatically). The broker awaits
``AskTracker.wait_for_answer`` and maps the typed ``Answer`` back to an ACP
``PermissionDecision``. ``ask.answer`` is the source of truth; the broker keeps
only a small per-request context map for the compat ``decide``/``get_pending``
surface and the ACP-specific event aliases.

Fail closed: only an explicit ``allowed`` outcome with a valid option becomes
``PermissionDecision(outcome="allowed")``; timeout / acknowledge / no-option
deny.
"""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any, Literal
from uuid import uuid4

from repowire.protocol.questions import Answer, Question, QuestionOption

if TYPE_CHECKING:
    from repowire.daemon.ask_tracker import AskTracker

PermissionOutcome = Literal["allowed", "denied", "cancelled"]

# Virtual recipient for a tool-permission question. Not a real peer and never
# delivered through MessageRouter — human surfaces answer it via /answer using
# the ask cid; a future roles/capabilities recipient model can replace it.
CONTROL_PEER_ID = "__repowire_control__"
CONTROL_PEER_NAME = "human"


@dataclass(frozen=True)
class PermissionDecision:
    """Resolved ACP permission decision."""

    outcome: PermissionOutcome
    option_id: str | None = None
    message: str | None = None
    timed_out: bool = False


@dataclass
class _PendingPermission:
    request_id: str  # == the ask correlation_id
    peer_id: str
    session_id: str
    tool_call: dict[str, Any]
    options: list[dict[str, Any]]


class ApprovalBroker:
    """ACP-protocol adapter: RequestPermission ↔ a blocking choice-question ask.

    Registers the question in the ``AskTracker`` and blocks on its answer waiter;
    the answer (from a human surface or a peer) resolves it. Defaults to deny on
    timeout. Emits the normal ask event (so the dashboard/Telegram renderers
    surface it) plus ``acp_permission_request``/``acp_permission_decision``
    aliases for back-compat/audit, keyed by the same request_id (= ask cid).
    """

    def __init__(
        self,
        *,
        emit_event: Callable[[str, dict[str, Any]], str],
        ask_tracker: AskTracker | None = None,
        resolve_repowire_session_id: Callable[[str, str], str | None] | None = None,
        timeout_seconds: float = 60.0,
    ) -> None:
        self._emit_event = emit_event
        self._ask_tracker = ask_tracker
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
        """Translate an ACP RequestPermission into a blocking question + await it.

        Registers a choice question ask (recipient = the virtual control surface)
        and blocks on its AskTracker answer waiter; a human/dashboard/peer answer
        resolves it. Timeout intentionally defaults to deny (fail closed), which
        keeps the ACP subprocess unblocked.
        """
        if self._ask_tracker is None:
            # No tracker wired (e.g. a bare test harness): fail closed.
            return PermissionDecision(outcome="denied", message="approval broker not wired")

        normalized_options = [_normalize_option(option) for option in options]
        request_id = f"acpperm-{uuid4().hex[:12]}"
        pending = _PendingPermission(
            request_id=request_id,
            peer_id=peer_id,
            session_id=session_id,
            tool_call=_normalize_payload(tool_call),
            options=normalized_options,
        )
        async with self._lock:
            self._pending[request_id] = pending

        question = Question(
            kind="choice",
            prompt=_tool_prompt(pending.tool_call),
            options=[_to_question_option(o) for o in normalized_options],
            blocking=True,
            timeout_seconds=self._timeout_seconds,
            default_answer=Answer(outcome="timed_out", message="permission request timed out"),
            scope="tool_permission",
            metadata={
                "acp_peer_id": peer_id,
                "acp_session_id": session_id,
                "tool_call": pending.tool_call,
                "request_id": request_id,
            },
        )
        repowire_session_id = self._resolve_session_id(peer_id, session_id)
        try:
            await self._ask_tracker.register(
                from_peer_id=peer_id,
                from_peer_name=peer_id,
                to_peer_id=CONTROL_PEER_ID,
                to_peer_name=CONTROL_PEER_NAME,
                text=question.prompt or "ACP tool permission request",
                correlation_id=request_id,
                question=question,
            )
        except Exception as e:  # noqa: BLE001 — registration must not hard-fail the ACP turn
            async with self._lock:
                self._pending.pop(request_id, None)
            return PermissionDecision(outcome="denied", message=f"register failed: {e}")

        # Normal ask event (so the dashboard banner + Telegram render it) + the
        # back-compat ACP-specific alias, both keyed by the same request_id/cid.
        ask_event = {
            "from": peer_id,
            "to": CONTROL_PEER_NAME,
            "from_peer_id": peer_id,
            "to_peer_id": CONTROL_PEER_ID,
            "text": question.prompt or "ACP tool permission request",
            "correlation_id": request_id,
            "question": question.model_dump(exclude_none=True),
            "repowire_session_id": repowire_session_id,
            "from_repowire_session_id": repowire_session_id,
        }
        self._emit_event("ask", ask_event)
        self._emit_event(
            "acp_permission_request",
            {
                "request_id": request_id,
                "peer_id": peer_id,
                "session_id": session_id,
                "repowire_session_id": repowire_session_id,
                "tool_call": pending.tool_call,
                "options": normalized_options,
                "status": "pending",
                "timeout_seconds": self._timeout_seconds,
            },
        )

        answer = await self._ask_tracker.wait_for_answer(
            request_id,
            timeout_seconds=self._timeout_seconds,
            default_answer=question.default_answer,
        )
        decision = _answer_to_decision(answer)
        if decision.timed_out:
            self._last_error = "permission request timed out"
            self._last_error_at = _utc_now()
        async with self._lock:
            self._pending.pop(request_id, None)
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
        """Back-compat resolver for POST /acp/permissions/{id}/decision.

        Maps the legacy outcome onto an Answer and resolves the underlying
        question ask. Returns ``None`` when the request is unknown/already
        resolved. ``ask.answer`` remains the source of truth.
        """
        async with self._lock:
            pending = self._pending.get(request_id)
        if pending is None or self._ask_tracker is None:
            return None
        if outcome == "allowed":
            option_id = option_id or _first_option_id(pending.options)
            answer = Answer(option_id=option_id, outcome="answered", message=message)
        elif outcome == "cancelled":
            answer = Answer(outcome="cancelled", message=message)
        else:
            answer = Answer(outcome="denied", message=message)  # first-class deny
        try:
            await self._ask_tracker.answer(request_id, answer)
        except self._ask_tracker.AlreadyAnsweredError:
            return None
        except (KeyError, ValueError):
            return None
        decision = PermissionDecision(outcome=outcome, option_id=option_id, message=message)
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


def _tool_prompt(tool_call: dict[str, Any]) -> str:
    """A short human prompt for the tool-permission question."""
    name = tool_call.get("title") or tool_call.get("name") or tool_call.get("kind") or "a tool"
    return f"Allow {name}?"


def _to_question_option(option: dict[str, Any]) -> QuestionOption:
    oid = str(option.get("option_id") or option.get("id") or option.get("name") or "option")
    title = str(option.get("name") or option.get("title") or oid)
    return QuestionOption(id=oid, title=title)


def _answer_to_decision(answer: Answer) -> PermissionDecision:
    """Map a typed Answer back to an ACP PermissionDecision. Fail closed.

    Only an explicit selected option becomes ``allowed``; timeout / acknowledge /
    cancel / no-option deny so a tool can never run on an ambiguous answer.
    """
    if answer.outcome == "timed_out":
        return PermissionDecision(outcome="denied", message=answer.message, timed_out=True)
    if answer.outcome == "cancelled":
        return PermissionDecision(outcome="cancelled", message=answer.message)
    if answer.outcome == "denied":
        return PermissionDecision(outcome="denied", message=answer.message)
    if answer.outcome == "answered" and answer.option_id:
        return PermissionDecision(
            outcome="allowed", option_id=answer.option_id, message=answer.message,
        )
    # acknowledged, or answered with no option → fail closed.
    return PermissionDecision(outcome="denied", option_id=answer.option_id, message=answer.message)


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
