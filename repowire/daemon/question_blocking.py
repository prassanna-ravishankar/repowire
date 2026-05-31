"""Neutral core for a blocking structured-question round-trip.

A *blocking* transport (an ACP runtime's RequestPermission, a Claude Code
PreToolUse hook, a future PreToolUse-equivalent) owns a suspendable call: it can
register a :class:`~repowire.protocol.questions.Question`, park, and resume once
the answer is recorded. This module is that shared policy center —
register the question ask, emit the normal ``ask`` event (so the dashboard
banner and Telegram buttons render it like any other question), await the
answer, and hand back the typed :class:`~repowire.protocol.questions.Answer`.

It is deliberately transport-neutral: no ACP ``_pending`` bookkeeping, no
``acp_permission_*`` aliases, no session/tool_call coupling. The ACP broker and
the ``POST /questions/ask-blocking`` route both call this so timeout / default /
fail-closed semantics live in exactly one place and cannot drift.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Protocol

from repowire.protocol.questions import Answer, Question

if TYPE_CHECKING:
    from repowire.daemon.ask_tracker import AskTracker

# Virtual recipient for a control-surface question (tool permission, approval).
# Not a real peer and never delivered through MessageRouter — human surfaces
# answer it via /answer using the ask cid; a future roles/capabilities recipient
# model can replace it.
CONTROL_PEER_ID = "__repowire_control__"
CONTROL_PEER_NAME = "human"


class _EmitEvent(Protocol):
    def __call__(self, event_type: str, data: dict[str, Any], /) -> str: ...


async def register_blocking_question_and_wait(
    ask_tracker: AskTracker,
    emit_event: _EmitEvent,
    *,
    question: Question,
    text: str,
    correlation_id: str,
    server_wait_seconds: float,
    from_peer_id: str,
    from_peer_name: str,
    to_peer_id: str = CONTROL_PEER_ID,
    to_peer_name: str = CONTROL_PEER_NAME,
    from_repowire_session_id: str | None = None,
    extra_ask_fields: dict[str, Any] | None = None,
) -> Answer:
    """Register a blocking question ask, emit its ``ask`` event, await the answer.

    The answer recorded on the ask is the source of truth. On timeout,
    :meth:`AskTracker.wait_for_answer` records the question's ``default_answer``
    (fail-closed for a tool-permission question), so callers always receive a
    terminal :class:`Answer` rather than a hang.

    ``correlation_id`` is supplied by the caller so a retrying transport reuses
    the same ask (registration is idempotent on the cid). ``extra_ask_fields``
    merges into the emitted ``ask`` event for transport-specific metadata
    (e.g. a runtime session id) without widening this function's contract.

    Registration failure propagates to the caller, which decides how to fail
    closed for its transport.
    """
    await ask_tracker.register(
        from_peer_id=from_peer_id,
        from_peer_name=from_peer_name,
        to_peer_id=to_peer_id,
        to_peer_name=to_peer_name,
        text=text,
        correlation_id=correlation_id,
        question=question,
        from_repowire_session_id=from_repowire_session_id,
    )

    ask_event: dict[str, Any] = {
        "from": from_peer_name,
        "to": to_peer_name,
        "from_peer_id": from_peer_id,
        "to_peer_id": to_peer_id,
        "text": text,
        "correlation_id": correlation_id,
        "question": question.model_dump(exclude_none=True),
    }
    if from_repowire_session_id is not None:
        ask_event["from_repowire_session_id"] = from_repowire_session_id
    if extra_ask_fields:
        ask_event.update(extra_ask_fields)
    emit_event("ask", ask_event)

    return await ask_tracker.wait_for_answer(
        correlation_id,
        timeout_seconds=server_wait_seconds,
        default_answer=question.default_answer,
    )
