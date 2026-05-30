"""Tests for the delivery trace ledger store."""

from __future__ import annotations

from repowire.daemon.delivery_trace import DeliveryTraceStore
from repowire.daemon.state.database import StateDatabase


def _store(tmp_path) -> DeliveryTraceStore:
    db = StateDatabase(tmp_path / "state.db")
    return DeliveryTraceStore(db)


def test_stages_ordered_by_seq_not_insertion(tmp_path):
    store = _store(tmp_path)
    # Record the canonical order; seq is assigned monotonically per trace.
    for stage in ("created", "resolved_peer", "routed", "websocket_sent", "pane_injected"):
        store.record(trace_id="ask-1", kind="ask", stage=stage, peer_id="p1")
    rows = store.stages_for("ask-1")
    assert [r.stage for r in rows] == [
        "created",
        "resolved_peer",
        "routed",
        "websocket_sent",
        "pane_injected",
    ]
    assert [r.seq for r in rows] == [0, 1, 2, 3, 4]


def test_traces_are_isolated_by_trace_id(tmp_path):
    store = _store(tmp_path)
    store.record(trace_id="ask-1", kind="ask", stage="created", peer_id="p1")
    store.record(trace_id="notif-2", kind="notify", stage="created", peer_id="p2")
    store.record(trace_id="ask-1", kind="ask", stage="routed", peer_id="p1")
    assert len(store.stages_for("ask-1")) == 2
    assert len(store.stages_for("notif-2")) == 1
    # seq is per-trace, so each trace starts at 0
    assert store.stages_for("notif-2")[0].seq == 0


def test_notify_uses_delivery_id(tmp_path):
    store = _store(tmp_path)
    store.record(
        trace_id="notif-deliv-abc",
        delivery_id="notif-deliv-abc",
        kind="notify",
        stage="websocket_sent",
        peer_id="p3",
    )
    row = store.stages_for("notif-deliv-abc")[0]
    assert row.delivery_id == "notif-deliv-abc"
    assert row.kind == "notify"


def test_failure_stage_recorded(tmp_path):
    store = _store(tmp_path)
    store.record(
        trace_id="ask-9",
        kind="ask",
        stage="injection_failed",
        status="fail",
        peer_id="p1",
        detail={"reason": "pane gone"},
    )
    row = store.stages_for("ask-9")[0]
    assert row.status == "fail"
    assert row.detail == {"reason": "pane gone"}


def test_latest_stage_returns_newest(tmp_path):
    store = _store(tmp_path)
    store.record(trace_id="ask-a", kind="ask", stage="pane_injected", peer_id="p1")
    store.record(trace_id="ask-b", kind="ask", stage="pane_injected", peer_id="p1")
    latest = store.latest_stage(peer_id="p1", stage="pane_injected")
    assert latest is not None
    assert latest.peer_id == "p1"
    # No pane_injected for a different peer
    assert store.latest_stage(peer_id="other", stage="pane_injected") is None


def test_prune_removes_only_old_rows(tmp_path):
    store = _store(tmp_path)
    # Insert with explicitly old + new timestamps by writing directly.
    store.record(trace_id="old-1", kind="ask", stage="created", peer_id="p1")
    store.record(trace_id="new-1", kind="ask", stage="created", peer_id="p1")
    # Force the first row's ts to the past.
    store._conn.execute(
        "UPDATE delivery_traces SET ts = ? WHERE trace_id = ?",
        ("2000-01-01T00:00:00+00:00", "old-1"),
    )
    store._conn.commit()
    removed = store.prune("2020-01-01T00:00:00+00:00")
    assert removed == 1
    assert store.stages_for("old-1") == []
    assert len(store.stages_for("new-1")) == 1


def test_record_is_best_effort_on_error(tmp_path):
    store = _store(tmp_path)
    # Close the underlying connection to force an error; record must not raise.
    store._conn.close()
    store.record(trace_id="ask-x", kind="ask", stage="created", peer_id="p1")


async def test_lazy_repair_prunes_old_traces(tmp_path):
    import time
    from unittest.mock import MagicMock

    from repowire.config.models import Config
    from repowire.daemon.peer_registry import PeerRegistry
    from repowire.daemon.state.database import StateDatabase

    db = StateDatabase(tmp_path / "state.db")
    store = DeliveryTraceStore(db)
    store.record(trace_id="old", kind="ask", stage="created", peer_id="p1")
    store.record(trace_id="new", kind="ask", stage="created", peer_id="p1")
    store._conn.execute(
        "UPDATE delivery_traces SET ts = ? WHERE trace_id = ?",
        ("2000-01-01T00:00:00+00:00", "old"),
    )
    store._conn.commit()

    cfg = Config()
    cfg.daemon.prune_max_age_hours = 24
    registry = PeerRegistry(
        config=cfg,
        message_router=MagicMock(),
        query_tracker=None,
        transport=None,
        ask_tracker=None,
        state_db=db,
    )
    registry._last_repair = time.monotonic() - 3600  # force the repair to run
    await registry.lazy_repair()

    assert store.stages_for("old") == []
    assert len(store.stages_for("new")) == 1
