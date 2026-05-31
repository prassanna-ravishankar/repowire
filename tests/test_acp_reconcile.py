"""Restart reconciliation for in-flight ACP asks (codex ACP must-fix #2)."""

from __future__ import annotations

from pathlib import Path

from repowire.daemon.acp_reconcile import (
    ACP_ASK_OPERATION_KIND,
    reconcile_acp_inflight,
    record_acp_ask_operation,
    settle_acp_ask_operation,
)
from repowire.daemon.state import StateDatabase
from repowire.daemon.state.operations import SQLiteOperationStore
from repowire.daemon.state.queued_deliveries import SQLiteQueuedDeliveryStore


def _stores(
    tmp_path: Path,
) -> tuple[StateDatabase, SQLiteOperationStore, SQLiteQueuedDeliveryStore]:
    db = StateDatabase(tmp_path / "state.db")
    queue = SQLiteQueuedDeliveryStore(db, ttl_seconds=86400, max_per_peer=100)
    return db, SQLiteOperationStore(db), queue


def test_record_creates_running_operation(tmp_path: Path) -> None:
    db, ops, _queue = _stores(tmp_path)
    try:
        op_id = record_acp_ask_operation(
            ops,
            correlation_id="cid-1",
            from_peer_id="asker-id",
            from_peer_name="asker",
            to_peer_id="answerer-id",
            to_peer_name="answerer",
        )
        assert op_id is not None
        op = ops.get(op_id)
        assert op is not None
        assert op.kind == ACP_ASK_OPERATION_KIND
        assert op.state == "running"  # ready to be swept if the daemon dies now
        assert op.target["correlation_id"] == "cid-1"
        assert op.target["from_peer_id"] == "asker-id"
    finally:
        db.close()


def test_settle_marks_terminal_so_sweep_ignores_it(tmp_path: Path) -> None:
    db, ops, queue = _stores(tmp_path)
    try:
        op_id = record_acp_ask_operation(
            ops, correlation_id="cid-1", from_peer_id="asker-id",
            from_peer_name="asker", to_peer_id="answerer-id", to_peer_name="answerer",
        )
        settle_acp_ask_operation(ops, op_id, error=None)  # successful ack
        assert ops.get(op_id).state == "completed"

        reconciled = reconcile_acp_inflight(ops, queue)
        assert reconciled == 0  # terminal op is not swept
        assert queue.count_for_peer("asker-id") == 0
    finally:
        db.close()


def test_reconcile_fails_inflight_and_enqueues_closure(tmp_path: Path) -> None:
    db, ops, queue = _stores(tmp_path)
    try:
        op_id = record_acp_ask_operation(
            ops, correlation_id="cid-lost", from_peer_id="asker-id",
            from_peer_name="asker", to_peer_id="answerer-id", to_peer_name="answerer",
        )
        # Daemon "restarts" with this op still running.
        reconciled = reconcile_acp_inflight(ops, queue)
        assert reconciled == 1
        assert ops.get(op_id).state == "failed"

        # The asker gets exactly one durable closure delivery.
        assert queue.count_for_peer("asker-id") == 1
        drained = queue.drain_for_peer("asker-id")
        assert len(drained) == 1
        assert "cid-lost" in drained[0].text
        assert "lost across daemon restart" in drained[0].text
    finally:
        db.close()


def test_reconcile_is_idempotent_across_restarts(tmp_path: Path) -> None:
    db, ops, queue = _stores(tmp_path)
    try:
        record_acp_ask_operation(
            ops, correlation_id="cid-lost", from_peer_id="asker-id",
            from_peer_name="asker", to_peer_id="answerer-id", to_peer_name="answerer",
        )
        first = reconcile_acp_inflight(ops, queue)
        second = reconcile_acp_inflight(ops, queue)  # a second startup
        assert first == 1
        assert second == 0  # already terminal -> no duplicate closure
        assert queue.count_for_peer("asker-id") == 1
    finally:
        db.close()


def test_reconcile_without_asker_id_still_fails_op(tmp_path: Path) -> None:
    # If we somehow lack the asker peer_id, the op must still be failed (audit),
    # we just can't enqueue a closure.
    db, ops, queue = _stores(tmp_path)
    try:
        op = ops.create(
            kind=ACP_ASK_OPERATION_KIND,
            target={"correlation_id": "cid-x", "to_peer_name": "answerer"},
        )
        ops.start_attempt(op.operation_id, strategy="acp_prompt")
        reconciled = reconcile_acp_inflight(ops, queue)
        assert reconciled == 1
        assert ops.get(op.operation_id).state == "failed"
    finally:
        db.close()
