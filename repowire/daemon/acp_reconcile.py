"""Durable lifecycle + restart reconciliation for ACP-routed asks.

An ACP ask is delivered by a daemon-owned background prompt task; its closure
(ack reply or error frame) only fires when that task completes. The AskTracker
is in-memory, so a daemon restart loses both the open ask and the task — and
unlike a WS peer, an ACP peer has no Stop hook to resurface it. The asker would
be left waiting forever.

To stay fail-loud we record each ACP ask as a durable ``acp_ask`` operation
(``SQLiteOperationStore``) created *before* the prompt task is scheduled, marked
terminal when the ask settles. On startup any still-running ``acp_ask`` op is a
task lost across the restart: we fail it and enqueue a single durable closure
delivery (``SQLiteQueuedDeliveryStore``) so the asker hears "your ACP ask was
lost, please retry" once their transport is back.

This is deliberately not a second redelivery system: same-peer_id reconnect gets
the closure via the existing queue; a changed peer_id may miss it but the
operation audit still records the failure.
"""

from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

ACP_ASK_OPERATION_KIND = "acp_ask"
_NON_TERMINAL_STATES = ("queued", "running")
_RESTART_LOST_REASON = "daemon_restart_lost_acp_task"


def record_acp_ask_operation(
    operation_store: Any,
    *,
    correlation_id: str,
    from_peer_id: str | None,
    from_peer_name: str,
    to_peer_id: str,
    to_peer_name: str,
) -> str | None:
    """Create a durable ``acp_ask`` operation before the prompt task is scheduled.

    Returns the operation_id (or None if no store / on failure — recording must
    never break delivery). The asker identity is persisted in ``target`` so a
    startup sweep can enqueue closure even after the in-memory ask is gone.
    """
    if operation_store is None:
        return None
    try:
        op = operation_store.create(
            kind=ACP_ASK_OPERATION_KIND,
            target={
                "correlation_id": correlation_id,
                "from_peer_id": from_peer_id,
                "from_peer_name": from_peer_name,
                "to_peer_id": to_peer_id,
                "to_peer_name": to_peer_name,
            },
        )
        # Move to running immediately: the prompt task is about to be scheduled,
        # so a crash after this point is a "lost task" the sweep must catch.
        operation_store.start_attempt(op.operation_id, strategy="acp_prompt")
        return op.operation_id
    except Exception as e:  # noqa: BLE001 — durability is best-effort, never block the ask
        logger.warning("Failed to record acp_ask operation for cid=%s: %s", correlation_id, e)
        return None


def settle_acp_ask_operation(
    operation_store: Any,
    operation_id: str | None,
    *,
    error: str | None,
) -> None:
    """Mark an ``acp_ask`` operation terminal once the ask settles.

    ``error`` None → completed; non-None → failed. Best-effort.
    """
    if operation_store is None or operation_id is None:
        return
    try:
        if error is None:
            operation_store.complete(operation_id, result={"outcome": "acked"})
        else:
            operation_store.fail(operation_id, error={"reason": "acp_error", "detail": error})
    except Exception as e:  # noqa: BLE001 — settling is best-effort
        logger.warning("Failed to settle acp_ask operation %s: %s", operation_id, e)


def reconcile_acp_inflight(operation_store: Any, queued_delivery_store: Any) -> int:
    """Fail every non-terminal ``acp_ask`` op and enqueue one closure per asker.

    Called once on daemon startup, after the stores are initialised. Idempotent:
    only ``queued``/``running`` ops are touched, and failing each op moves it to
    a terminal state so a second startup is a no-op. Returns the count reconciled.
    """
    if operation_store is None:
        return 0
    reconciled = 0
    for state in _NON_TERMINAL_STATES:
        try:
            ops = operation_store.list_all(kind=ACP_ASK_OPERATION_KIND, state=state)
        except Exception as e:  # noqa: BLE001
            logger.warning("acp reconcile: list_all(%s) failed: %s", state, e)
            continue
        for op in ops:
            target = op.target or {}
            cid = target.get("correlation_id", "?")
            from_peer_id = target.get("from_peer_id")
            from_peer_name = target.get("from_peer_name", from_peer_id or "peer")
            to_peer_id = target.get("to_peer_id")
            to_peer_name = target.get("to_peer_name", "peer")
            try:
                operation_store.fail(op.operation_id, error={"reason": _RESTART_LOST_REASON})
            except Exception as e:  # noqa: BLE001
                logger.warning("acp reconcile: fail(%s) failed: %s", op.operation_id, e)
                continue
            reconciled += 1
            if queued_delivery_store is None or not from_peer_id:
                logger.warning(
                    "acp reconcile: cid=%s lost across restart; no closure enqueued "
                    "(queue=%s, from_peer_id=%s)",
                    cid, queued_delivery_store is not None, bool(from_peer_id),
                )
                continue
            closure = (
                f"[ack #{cid} from @{to_peer_name}] "
                "ACP ask lost across daemon restart; please retry."
            )
            try:
                # The closure is FROM the answerer (the ACP peer that was asked)
                # TO the original asker (peer_id = the queue key).
                queued_delivery_store.enqueue(
                    peer_id=from_peer_id,
                    repowire_session_id=None,
                    kind="notify",
                    from_peer_id=to_peer_id,
                    from_peer_name=to_peer_name,
                    to_peer_name=from_peer_name,
                    text=closure,
                    attachments=[],
                )
            except Exception as e:  # noqa: BLE001
                logger.warning("acp reconcile: enqueue closure for cid=%s failed: %s", cid, e)
    if reconciled:
        logger.info("acp reconcile: failed + queued closure for %d lost ACP ask(s)", reconciled)
    return reconciled
