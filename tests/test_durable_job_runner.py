from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.job_runner import JobRunner
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import work as work_routes
from repowire.daemon.spawn_service import SpawnService, SpawnServiceResult
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.work import SQLiteWorkStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import PeerRole, PeerStatus


class FakeDelivery:
    def __init__(self, cid: str = "ask-test") -> None:
        self.cid = cid
        self.calls: list[dict] = []
        self.fail: Exception | None = None

    async def open_scheduled_ask(self, **kwargs):
        self.calls.append(kwargs)
        if self.fail:
            raise self.fail
        return self.cid


class FakeSpawn:
    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.fail: Exception | None = None

    def spawn(self, **kwargs):
        self.calls.append(kwargs)
        if self.fail:
            raise self.fail
        return SpawnServiceResult(
            display_name="worker",
            tmux_session="jobs:worker",
            pane_id="%9",
            message=kwargs.get("message"),
        )


def _env(tmp_path: Path):
    cfg = Config()
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=qt,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    db = StateDatabase(tmp_path / "state.db")
    store = SQLiteWorkStore(db)
    delivery = FakeDelivery()
    spawn = FakeSpawn()
    runner = JobRunner(
        config=cfg,
        work_store=store,
        peer_registry=registry,
        peer_delivery=delivery,  # type: ignore[arg-type]
        spawn_service=spawn,  # type: ignore[arg-type]
        poll_interval=0.01,
    )
    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        work_store=store,
        job_runner=runner,
        relay_mode=False,
    )
    init_deps(cfg, registry, state)
    return cfg, registry, db, store, delivery, spawn, runner


async def _register_peer(
    registry: PeerRegistry,
    *,
    peer_id: str = "repow-default-worker",
    status=PeerStatus.ONLINE,
):
    pid, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/project",
        peer_id=peer_id,
        initial_status=status,
        role=PeerRole.AGENT,
    )
    return pid


def test_atomic_acquire_prevents_double_dispatch(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, _runner = _env(tmp_path)
    work = store.create(title="job")

    first = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat(),
    )
    second = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r2",
        lease_until=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat(),
    )

    assert first is not None
    assert second is None
    assert store.get(work.work_id).state == "dispatching"
    db.close()
    cleanup_deps()


def test_startup_recovery_marks_stale_dispatching_unavailable(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    work = store.create(title="job")
    store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) - timedelta(seconds=1)).isoformat(),
    )

    recovered = runner.recover_stale()

    assert [w.work_id for w in recovered] == [work.work_id]
    status = store.get(work.work_id)
    assert status.state == "unavailable"
    assert status.state_reason == "runner_interrupted"
    assert status.error == {"reason": "runner_interrupted"}
    db.close()
    cleanup_deps()


def test_startup_recovery_marks_stale_delivered_unavailable(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    work = store.create(title="job")
    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) - timedelta(seconds=1)).isoformat(),
    )
    attempt_id = acquired.provenance["runner"]["current_attempt_id"]
    store.update_attempt(
        work.work_id,
        attempt_id=attempt_id,
        status="delivered",
        phase="delivered",
        correlation_id="ask-1",
    )

    recovered = runner.recover_stale()

    assert [w.work_id for w in recovered] == [work.work_id]
    assert store.get(work.work_id).state == "unavailable"
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_live_runner_recovers_delivered_after_lease_without_restart(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    work = store.create(title="job")
    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) + timedelta(milliseconds=50)).isoformat(),
    )
    attempt_id = acquired.provenance["runner"]["current_attempt_id"]
    store.update_attempt(
        work.work_id,
        attempt_id=attempt_id,
        status="delivered",
        phase="delivered",
        correlation_id="ask-1",
    )

    await runner.start()
    try:
        await asyncio.sleep(0.2)
        recovered = store.get(work.work_id)
        assert recovered.state == "unavailable"
        assert recovered.state_reason == "runner_interrupted"
    finally:
        await runner.stop()
        db.close()
        cleanup_deps()


def test_running_heartbeat_avoids_delivered_lease_recovery(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    work = store.create(title="job")
    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) - timedelta(seconds=1)).isoformat(),
    )
    attempt_id = acquired.provenance["runner"]["current_attempt_id"]
    store.update_attempt(
        work.work_id,
        attempt_id=attempt_id,
        status="delivered",
        phase="delivered",
        correlation_id="ask-1",
    )
    store.update_state(work.work_id, state="running", attempt_id=attempt_id)

    recovered = runner.recover_stale()

    assert recovered == []
    assert store.get(work.work_id).state == "running"
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_wake_set_during_deadline_computation_is_not_lost(tmp_path, monkeypatch):
    _cfg, _registry, db, _store, _delivery, _spawn, runner = _env(tmp_path)
    calls = 0

    async def fake_run_due_once():
        nonlocal calls
        calls += 1
        if calls >= 2:
            runner._stopped.set()  # noqa: SLF001
        return []

    def fake_next_deadline():
        if calls == 1:
            runner.wake()
        return None

    monkeypatch.setattr(runner, "run_due_once", fake_run_due_once)
    monkeypatch.setattr(runner, "_seconds_until_next_deadline", fake_next_deadline)

    await runner.start()
    await asyncio.wait_for(runner._task, timeout=1.0)  # noqa: SLF001

    assert calls >= 2
    db.close()
    cleanup_deps()


def test_runner_waits_indefinitely_when_no_due_or_lease_deadline(tmp_path):
    _cfg, _registry, db, _store, _delivery, _spawn, runner = _env(tmp_path)

    assert runner._seconds_until_next_deadline() is None  # noqa: SLF001

    db.close()
    cleanup_deps()


def test_due_at_offset_is_compared_by_instant_not_string(tmp_path):
    _cfg, _registry, db, store, _delivery, _spawn, _runner = _env(tmp_path)
    # Lexicographically less than a UTC now string on many days, but far in the future by instant.
    future = (datetime.now(timezone.utc) + timedelta(days=1)).astimezone(
        timezone(timedelta(hours=-10))
    )
    work = store.create(
        title="future",
        request={"execution": {"schedule": {"due_at": future.isoformat()}}},
    )

    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat(),
        ignore_due_at=False,
    )

    assert acquired is None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_manual_run_future_due_dispatches_once_and_records_correlation(tmp_path):
    _cfg, registry, db, store, delivery, _spawn, runner = _env(tmp_path)
    peer_id = await _register_peer(registry)
    future = (datetime.now(timezone.utc) + timedelta(days=1)).isoformat()
    work = store.create(
        title="future",
        assigned_peer_id=peer_id,
        circle="default",
        request={
            "execution": {
                "prompt": {"body": "do it", "source": "inline"},
                "target": {"assigned_peer_id": peer_id},
                "schedule": {"due_at": future},
                "delivery": {"kind": "ask"},
            }
        },
    )

    skipped = await runner.run_due_once()
    ran = await runner.run_job(work.work_id, ignore_due_at=True)
    again = await runner.run_job(work.work_id, ignore_due_at=True)

    assert skipped == []
    assert again is None
    assert ran.state == "delivered"
    assert len(delivery.calls) == 1
    assert delivery.calls[0]["to_peer"] == peer_id
    assert "attempt_id:" in delivery.calls[0]["text"]
    assert store.get(work.work_id).correlation_id == "ask-test"
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_cancel_during_acquired_before_delivery_records_cancelled(tmp_path):
    _cfg, registry, db, store, delivery, _spawn, runner = _env(tmp_path)
    peer_id = await _register_peer(registry)
    work = store.create(
        title="cancel",
        assigned_peer_id=peer_id,
        circle="default",
        request={"execution": {"target": {"assigned_peer_id": peer_id}}},
    )
    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat(),
    )
    attempt_id = acquired.provenance["runner"]["current_attempt_id"]
    store.cancel(work.work_id, reason="user_requested")

    result = await runner._dispatch(store.get(work.work_id), attempt_id)  # noqa: SLF001

    assert result.state == "cancelled"
    assert delivery.calls == []
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_retry_preserves_attempts_and_stale_update_conflicts(tmp_path):
    _cfg, registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    peer_id = await _register_peer(registry)
    work = store.create(
        title="retry",
        assigned_peer_id=peer_id,
        circle="default",
        request={"execution": {"target": {"assigned_peer_id": peer_id}}},
    )
    first = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat(),
    )
    old_attempt = first.provenance["runner"]["current_attempt_id"]
    store.update_attempt(
        work.work_id,
        attempt_id=old_attempt,
        status="failed",
        phase="delivery",
        error={"reason": "ask_delivery_failed"},
    )

    retried = await runner.run_job(work.work_id, retry=True)
    new_attempt = retried.provenance["runner"]["current_attempt_id"]

    assert new_attempt != old_attempt
    assert retried.provenance["runner"]["attempt_count"] == 2
    with pytest.raises(RuntimeError, match="stale_attempt"):
        store.update_state(work.work_id, state="completed", attempt_id=old_attempt)
    stale_update = store.update_attempt(
        work.work_id,
        attempt_id=old_attempt,
        status="failed",
        phase="old-failed",
        correlation_id="ask-old",
        error={"reason": "old_attempt_failed"},
    )
    assert stale_update.state == "delivered"
    assert stale_update.phase == "delivered"
    assert stale_update.correlation_id != "ask-old"
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_retry_from_delivered_creates_new_attempt(tmp_path):
    _cfg, registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    peer_id = await _register_peer(registry)
    work = store.create(
        title="retry delivered",
        assigned_peer_id=peer_id,
        circle="default",
        request={"execution": {"target": {"assigned_peer_id": peer_id}}},
    )
    delivered = await runner.run_job(work.work_id)
    old_attempt = delivered.provenance["runner"]["current_attempt_id"]

    retried = await runner.run_job(work.work_id, retry=True)

    assert retried.state == "delivered"
    assert retried.provenance["runner"]["attempt_count"] == 2
    assert retried.provenance["runner"]["current_attempt_id"] != old_attempt
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_offline_explicit_peer_becomes_unavailable(tmp_path):
    _cfg, registry, db, store, _delivery, _spawn, runner = _env(tmp_path)
    peer_id = await _register_peer(registry, status=PeerStatus.OFFLINE)
    work = store.create(
        title="offline",
        assigned_peer_id=peer_id,
        circle="default",
        request={"execution": {"target": {"assigned_peer_id": peer_id}}},
    )

    result = await runner.run_job(work.work_id)

    assert result.state == "unavailable"
    assert result.error == {"reason": "assigned_peer_offline"}
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_create_ambiguous_display_name_returns_409(tmp_path):
    _cfg, registry, db, _store, _delivery, _spawn, runner = _env(tmp_path)
    peer_a, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/same",
        peer_id="repow-default-a",
        initial_status=PeerStatus.ONLINE,
    )
    peer_b, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/tmp/same",
        peer_id="repow-default-b",
        initial_status=PeerStatus.ONLINE,
    )
    # Force ambiguity; allocator normally suffixes display names.
    registry._peers[peer_a].display_name = "same"  # noqa: SLF001
    registry._peers[peer_b].display_name = "same"  # noqa: SLF001
    app = FastAPI()
    app.include_router(work_routes.router)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post("/jobs", json={"title": "x", "assigned_peer_id": "same"})

    assert response.status_code == 409
    await runner.stop()
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_spawn_service_records_ownership_and_warmup(tmp_path, monkeypatch):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CLAUDE_CODE] = "claude"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    spawned: set[str] = set()
    tasks: set[asyncio.Task] = set()

    async def warmup(*_args, **_kwargs):
        return None

    result_obj = SimpleNamespace(
        display_name="proj",
        tmux_session="jobs:proj",
        pane_id="%1",
        message="warm",
    )
    spawn_impl = Mock(return_value=result_obj)
    monkeypatch.setattr("repowire.daemon.spawn_service.record_spawn_ownership", Mock())
    service = SpawnService(
        config=cfg,
        spawned_pane_ids=spawned,
        background_tasks=tasks,
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    result = service.spawn(path=str(tmp_path), backend=AgentType.CLAUDE_CODE, message="warm")

    assert result.pane_id == "%1"
    assert "%1" in spawned
    assert tasks
