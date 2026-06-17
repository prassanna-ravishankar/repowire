from __future__ import annotations

import asyncio
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.agent_backends import (
    AGENT_BACKENDS,
    DEFAULT_SPAWN_COMMANDS,
    AgentResumePlan,
    can_resume_backend,
    resume_capability_for_registration,
)
from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.job_completion import JobCompletionService
from repowire.daemon.job_runner import JobRunner
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import work as work_routes
from repowire.daemon.session_control import SessionControlService
from repowire.daemon.spawn_service import (
    SpawnRegistrationResult,
    SpawnService,
    SpawnServiceResult,
    capture_login_shell_path,
    command_head_for_probe,
)
from repowire.daemon.state.calendar import SQLiteCalendarStore
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.operations import SQLiteOperationStore
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
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
        self.display_name = "worker"
        self.tmux_session = "jobs:worker"
        self.pane_id = "%9"

    def spawn(self, **kwargs):
        self.calls.append(kwargs)
        if self.fail:
            raise self.fail
        return SpawnServiceResult(
            display_name=self.display_name,
            tmux_session=self.tmux_session,
            pane_id=self.pane_id,
            message=kwargs.get("message"),
        )

    async def spawn_registered(self, *, await_peer, attempts=2, on_spawned=None, **kwargs):
        registration_attempts: list[dict] = []
        spawn_result = None
        for attempt in range(1, attempts + 1):
            spawn_result = self.spawn(**kwargs)
            if on_spawned is not None:
                on_spawned(spawn_result)
            resolved = await await_peer(spawn_result)
            if resolved is not None:
                return SpawnRegistrationResult(
                    spawn_result=spawn_result,
                    resolved_peer=resolved,
                    registration_attempts=registration_attempts,
                )
            registration_attempts.append({"attempt": attempt})
        return SpawnRegistrationResult(
            spawn_result=spawn_result,
            resolved_peer=None,
            registration_attempts=registration_attempts,
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
    calendar = SQLiteCalendarStore(db, store)
    operations = SQLiteOperationStore(db)
    session_bindings = SQLiteSessionBindingStore(db)
    delivery = FakeDelivery()
    spawn = FakeSpawn()
    session_control = SessionControlService(
        peer_registry=registry,
        spawn_service=spawn,  # type: ignore[arg-type]
        operation_store=operations,
        session_binding_store=session_bindings,
        calendar_store=calendar,
    )
    runner = JobRunner(
        config=cfg,
        work_store=store,
        calendar_store=calendar,
        peer_registry=registry,
        peer_delivery=delivery,  # type: ignore[arg-type]
        spawn_service=spawn,  # type: ignore[arg-type]
        session_binding_store=session_bindings,
        session_control=session_control,
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
        calendar_store=calendar,
        operation_store=operations,
        session_binding_store=session_bindings,
        session_control=session_control,
        job_runner=runner,
        relay_mode=False,
    )
    init_deps(cfg, registry, state)
    return cfg, registry, db, store, calendar, session_bindings, delivery, spawn, runner


async def _register_peer(
    registry: PeerRegistry,
    *,
    peer_id: str = "repow-default-worker",
    backend: AgentType = AgentType.CLAUDE_CODE,
    path: str = "/tmp/project",
    pane_id: str | None = None,
    tmux_session: str | None = None,
    metadata: dict | None = None,
    status=PeerStatus.ONLINE,
):
    pid, _ = await registry.allocate_and_register(
        circle="default",
        backend=backend,
        path=path,
        pane_id=pane_id,
        tmux_session=tmux_session,
        metadata=metadata,
        peer_id=peer_id,
        initial_status=status,
        role=PeerRole.AGENT,
    )
    return pid


def test_atomic_acquire_prevents_double_dispatch(tmp_path):
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, _runner = _env(
        tmp_path
    )
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
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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


def test_startup_recovery_leaves_delivered_fires_alone(tmp_path, monkeypatch):
    """Delivered fires belong to the executor — an expired lease must not
    reap them to terminal unavailable while the executor may still be working.
    Executor death is detected by the fire-completion service instead."""
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
    kill_pane = Mock(return_value=True)
    monkeypatch.setattr("repowire.daemon.session_control.kill_pane", kill_pane)
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
        acquisition={
            "release_handle": {
                "kind": "tmux_pane",
                "peer_id": "repow-default-worker",
                "pane_id": "%900",
            }
        },
    )

    recovered = runner.recover_stale()

    assert recovered == []
    assert store.get(work.work_id).state == "delivered"
    kill_pane.assert_not_called()
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_live_runner_leaves_delivered_alone_after_lease_expiry(tmp_path):
    """A delivered fire belongs to the executor: the runner's lease must not
    reap it to terminal unavailable while the executor may still be working
    (its late report would then 400 and the result would be lost)."""
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
        assert recovered.state == "delivered"
    finally:
        await runner.stop()
        db.close()
        cleanup_deps()


def test_running_heartbeat_avoids_delivered_lease_recovery(tmp_path):
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, _registry, db, _store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, _registry, db, _store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )

    assert runner._seconds_until_next_deadline() is None  # noqa: SLF001

    db.close()
    cleanup_deps()


def test_due_at_offset_is_compared_by_instant_not_string(tmp_path):
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, _spawn, _runner = _env(
        tmp_path
    )
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
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, _spawn, runner = _env(
        tmp_path
    )
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
async def test_path_backend_persistent_policy_reuses_live_matching_peer_without_spawning(
    tmp_path,
):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    peer_id = await _register_peer(
        registry,
        peer_id="repow-default-daily",
        backend=AgentType.CODEX,
        path=worker_path,
        pane_id="%42",
        tmux_session="default:daily-email-brief",
        metadata={"hook_session_id": "codex-session-1"},
        status=PeerStatus.ONLINE,
    )
    _session_bindings.upsert_observation(
        peer_id=peer_id,
        backend="codex",
        project_path=worker_path,
        runtime_session_id="codex-session-1",
        resume_capability={
            "supported": True,
            "strategy": "codex_resume",
            "runtime_session_id_arg": "codex-session-1",
        },
        status="active",
        metadata={"hook_session_id": "codex-session-1"},
    )
    work = store.create(
        title="daily brief",
        circle="default",
        request={
            "execution": {
                "process_scope": "persistent",
                "prompt": {"body": "prepare brief", "source": "inline"},
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )

    result = await runner.run_job(work.work_id)

    assert result.state == "delivered"
    assert spawn.calls == []
    assert delivery.calls[0]["to_peer"] == peer_id
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["assigned_peer_id"] == peer_id
    assert attempt["assigned_peer_info"]["path"] == worker_path
    runtime_binding = attempt["assigned_peer_info"]["runtime_binding"]
    assert runtime_binding["runtime_session_id"] == "codex-session-1"
    assert runtime_binding["resume_capability"]["strategy"] == "codex_resume"
    assert attempt["tmux"] == {"tmux_session": "default:daily-email-brief", "pane_id": "%42"}
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_path_backend_job_defaults_to_per_fire(tmp_path):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    live_peer_id = await _register_peer(
        registry,
        peer_id="repow-default-old-live",
        backend=AgentType.CODEX,
        path=worker_path,
        pane_id="%42",
        tmux_session="default:old-live",
        metadata={"hook_session_id": "codex-session-live"},
        status=PeerStatus.ONLINE,
    )
    work = store.create(
        title="daily brief",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
    )
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-per-fire",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-session-new"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        result = await runner.run_job(work.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert len(spawn.calls) == 1
    assert delivery.calls[0]["to_peer"] == registered["peer_id"]
    assert delivery.calls[0]["to_peer"] != live_peer_id
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "spawned_peer"
    assert attempt["acquisition"]["release_handle"]["pane_id"] == spawn.pane_id
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_session_control_retries_spawn_registration_timeout(tmp_path, monkeypatch):
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    work = store.create(
        title="daily brief",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
    )

    async def never_registered(*_args, **_kwargs):
        return None

    monkeypatch.setattr(
        "repowire.daemon.session_control.SessionControlService._await_spawned_peer",
        never_registered,
    )

    result = await runner.run_job(work.work_id)

    assert result.state == "unavailable"
    assert len(spawn.calls) == 2
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["error"]["reason"] == "spawned_peer_not_registered"
    assert [item["attempt"] for item in attempt["error"]["registration_attempts"]] == [1, 2]
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_legacy_runner_retries_spawn_registration_timeout(tmp_path, monkeypatch):
    _cfg, _registry, db, store, _calendar, _session_bindings, _delivery, spawn, runner = _env(
        tmp_path
    )
    runner._session_control = None
    worker_path = str(tmp_path / "daily-email-brief")
    work = store.create(
        title="daily brief",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
    )

    async def never_registered(*_args, **_kwargs):
        return None

    monkeypatch.setattr(
        "repowire.daemon.job_runner.JobRunner._await_spawned_peer",
        never_registered,
    )

    result = await runner.run_job(work.work_id)

    assert result.state == "unavailable"
    assert len(spawn.calls) == 2
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["error"]["reason"] == "spawned_peer_not_registered"
    assert [item["attempt"] for item in attempt["error"]["registration_attempts"]] == [1, 2]
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_legacy_recurring_path_backend_defaults_to_per_fire(tmp_path):
    _cfg, registry, db, _store, calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    live_peer_id = await _register_peer(
        registry,
        peer_id="repow-default-old-live",
        backend=AgentType.CODEX,
        path=worker_path,
        pane_id="%42",
        tmux_session="default:old-live",
        metadata={"hook_session_id": "codex-session-live"},
        status=PeerStatus.ONLINE,
    )
    calendar.create(
        title="daily brief",
        kind="brief",
        cron="*/2 * * * *",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )
    [child] = calendar.materialize_due(now=datetime(2026, 5, 25, 8, 2, tzinfo=timezone.utc))
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-per-fire",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-session-new"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        result = await runner.run_job(child.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert len(spawn.calls) == 1
    assert delivery.calls[0]["to_peer"] == registered["peer_id"]
    assert delivery.calls[0]["to_peer"] != live_peer_id
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "spawned_peer"
    assert attempt["acquisition"]["release_handle"]["pane_id"] == spawn.pane_id
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_recurring_path_backend_persistent_policy_reuses_live_peer(tmp_path):
    _cfg, registry, db, _store, calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    peer_id = await _register_peer(
        registry,
        peer_id="repow-default-daily",
        backend=AgentType.CODEX,
        path=worker_path,
        pane_id="%42",
        tmux_session="default:daily-email-brief",
        metadata={"hook_session_id": "codex-session-1"},
        status=PeerStatus.ONLINE,
    )
    calendar.create(
        title="daily brief",
        kind="brief",
        cron="*/2 * * * *",
        circle="default",
        request={
            "execution": {
                "process_scope": "persistent",
                "target": {"path": worker_path, "backend": "codex"},
            }
        },
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )
    [child] = calendar.materialize_due(now=datetime(2026, 5, 25, 8, 2, tzinfo=timezone.utc))

    result = await runner.run_job(child.work_id)

    assert result.state == "delivered"
    assert spawn.calls == []
    assert delivery.calls[0]["to_peer"] == peer_id
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "reused_peer"
    assert attempt["acquisition"]["release_handle"] is None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_spawned_peer_wait_tolerates_delayed_codex_registration(tmp_path):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    work = store.create(
        title="daily brief",
        circle="default",
        request={
            "execution": {
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        await asyncio.sleep(0.05)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-spawned",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-session-2"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        _session_bindings.upsert_observation(
            peer_id=peer_id,
            backend="codex",
            project_path=worker_path,
            runtime_session_id="codex-session-2",
            resume_capability={
                "supported": True,
                "strategy": "codex_resume",
                "runtime_session_id_arg": "codex-session-2",
            },
            status="active",
            metadata={"hook_session_id": "codex-session-2"},
        )
        registry._peers[peer_id].display_name = "worker"  # noqa: SLF001
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        result = await runner.run_job(work.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert len(spawn.calls) == 1
    assert delivery.calls[0]["to_peer"] == registered["peer_id"]
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["assigned_peer_id"] == registered["peer_id"]
    assert attempt["assigned_peer_info"]["display_name"] == "worker"
    assert (
        attempt["assigned_peer_info"]["runtime_binding"]["runtime_session_id"] == "codex-session-2"
    )
    assert attempt["tmux"] == {
        "tmux_session": "default:daily-email-brief",
        "pane_id": spawn.pane_id,
    }
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_spawned_peer_wait_matches_pane_when_display_name_differs(tmp_path):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    spawn.display_name = "daily-email-brief-2"
    spawn.tmux_session = "default:daily-email-brief-2"
    spawn.pane_id = "%777"
    work = store.create(
        title="daily brief",
        circle="default",
        request={
            "execution": {
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    registered: dict[str, str] = {}

    async def register_with_path_name() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        await asyncio.sleep(0.05)
        peer_id, display_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            pane_id="%777",
            tmux_session="default:daily-email-brief-2",
            metadata={"hook_session_id": "codex-session-3"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id
        registered["display_name"] = display_name

    task = asyncio.create_task(register_with_path_name())
    try:
        result = await runner.run_job(work.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert registered["display_name"] != spawn.display_name
    assert delivery.calls[0]["to_peer"] == registered["peer_id"]
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["assigned_peer_id"] == registered["peer_id"]
    assert attempt["assigned_peer_info"]["display_name"] == registered["display_name"]
    assert attempt["tmux"] == {"tmux_session": "default:daily-email-brief-2", "pane_id": "%777"}
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_cancel_during_acquired_before_delivery_records_cancelled(tmp_path):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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
    _cfg, registry, db, _store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
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


def test_calendar_materializes_one_child_and_advances_next_due(tmp_path):
    _cfg, _registry, db, store, calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
    base = datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc)
    entry = calendar.create(
        title="daily brief",
        kind="brief",
        cron="0 8 * * *",
        request={"execution": {"prompt": {"body": "brief", "source": "inline"}}},
        now=base - timedelta(days=1),
    )

    materialized = calendar.materialize_due(now=base + timedelta(minutes=1))
    second = calendar.materialize_due(now=base + timedelta(minutes=1))

    assert len(materialized) == 1
    assert second == []
    child = store.get(materialized[0].work_id)
    assert child.source_kind == "calendar"
    assert child.source_id == entry.calendar_id
    assert child.provenance["calendar"]["calendar_id"] == entry.calendar_id
    refreshed = calendar.get(entry.calendar_id)
    assert refreshed.last_occurrence_work_id == child.work_id
    assert _parse_iso_for_test(refreshed.next_due_at) > base + timedelta(minutes=1)
    db.close()
    cleanup_deps()


def test_calendar_records_latest_runtime_binding(tmp_path):
    _cfg, _registry, db, _store, calendar, _session_bindings, _delivery, _spawn, _runner = _env(
        tmp_path
    )
    entry = calendar.create(
        title="daily brief",
        kind="brief",
        cron="@daily",
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )

    updated = calendar.update_runtime_binding(
        entry.calendar_id,
        binding={
            "peer_id": "repow-default-daily",
            "backend": "codex",
            "path": "/repo",
            "circle": "default",
            "runtime_session_id": "codex-runtime-1",
            "resume_capability": {"supported": True, "strategy": "codex_resume"},
        },
    )

    assert updated.provenance["runtime_binding"]["runtime_session_id"] == "codex-runtime-1"
    assert updated.provenance["runtime_binding_history"][0]["peer_id"] == "repow-default-daily"
    db.close()
    cleanup_deps()


def test_calendar_missed_runs_coalesce_to_one_occurrence(tmp_path):
    _cfg, _registry, db, store, calendar, _session_bindings, _delivery, _spawn, _runner = _env(
        tmp_path
    )
    entry = calendar.create(
        title="hourly",
        kind="brief",
        cron="@hourly",
        now=datetime(2026, 5, 25, 0, 0, tzinfo=timezone.utc),
    )

    materialized = calendar.materialize_due(now=datetime(2026, 5, 25, 5, 30, tzinfo=timezone.utc))

    assert len(materialized) == 1
    assert len(store.list_all()) == 1
    assert _parse_iso_for_test(calendar.get(entry.calendar_id).next_due_at) == datetime(
        2026, 5, 25, 6, 0, tzinfo=timezone.utc
    )
    db.close()
    cleanup_deps()


def test_calendar_cancel_prevents_future_materialization(tmp_path):
    _cfg, _registry, db, store, calendar, _session_bindings, _delivery, _spawn, _runner = _env(
        tmp_path
    )
    entry = calendar.create(
        title="cancel recurring",
        kind="brief",
        cron="@hourly",
        now=datetime(2026, 5, 25, 0, 0, tzinfo=timezone.utc),
    )

    cancelled = calendar.cancel(entry.calendar_id, reason="user_requested")
    materialized = calendar.materialize_due(now=datetime(2026, 5, 25, 2, 0, tzinfo=timezone.utc))

    assert cancelled.state == "cancelled"
    assert materialized == []
    assert store.list_all() == []
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_runner_materializes_and_dispatches_due_calendar_child(tmp_path):
    _cfg, registry, db, store, calendar, _session_bindings, delivery, _spawn, runner = _env(
        tmp_path
    )
    peer_id = await _register_peer(registry)
    calendar.create(
        title="daily brief",
        kind="brief",
        cron="0 8 * * *",
        assigned_peer_id=peer_id,
        circle="default",
        request={"execution": {"target": {"assigned_peer_id": peer_id}}},
        now=datetime(2026, 5, 25, 7, 0, tzinfo=timezone.utc),
    )

    runner.materialize_due_calendar()
    dispatched = await runner.run_due_once()

    assert len(store.list_all()) == 1
    assert dispatched == [store.list_all()[0].work_id]
    assert store.list_all()[0].state == "delivered"
    assert len(delivery.calls) == 1
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_recurring_job_uses_recorded_codex_resume_binding(tmp_path, monkeypatch):
    _cfg, registry, db, store, calendar, _session_bindings, delivery, spawn, runner = _env(tmp_path)
    worker_path = str(tmp_path / "daily-email-brief")
    # Resume now pre-validates the session id on disk (jobs share the restart
    # safety seam). Create a matching codex rollout so the recorded binding is
    # genuinely resumable, under a mocked HOME so we don't touch real state.
    fake_home = tmp_path / "home"
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: fake_home))
    _codex_rollout_dir = fake_home / ".codex" / "sessions" / "2026" / "05" / "25"
    _codex_rollout_dir.mkdir(parents=True, exist_ok=True)
    (_codex_rollout_dir / "rollout-2026-05-25T08-00-00-codex-runtime-old.jsonl").write_text(
        '{"type":"session_meta","payload":{"id":"codex-runtime-old","cwd":'
        f'"{worker_path}"}}}}\n'
    )
    entry = calendar.create(
        title="daily brief",
        kind="brief",
        cron="*/2 * * * *",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
        provenance={
            "runtime_binding": {
                "peer_id": "repow-default-old",
                "backend": "codex",
                "path": worker_path,
                "circle": "default",
                "runtime_session_id": "codex-runtime-old",
                "resume_capability": {
                    "supported": True,
                    "strategy": "codex_resume",
                    "runtime_session_id_arg": "codex-runtime-old",
                },
            }
        },
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )
    [child] = calendar.materialize_due(now=datetime(2026, 5, 25, 8, 2, tzinfo=timezone.utc))
    resumed: dict[str, str] = {}

    async def register_after_resume_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        await asyncio.sleep(0.05)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-resumed",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-runtime-old"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        resumed["peer_id"] = peer_id
        _session_bindings.upsert_observation(
            peer_id=peer_id,
            backend="codex",
            project_path=worker_path,
            runtime_session_id="codex-runtime-old",
            resume_capability={
                "supported": True,
                "strategy": "codex_resume",
                "runtime_session_id_arg": "codex-runtime-old",
            },
            status="active",
            metadata={"hook_session_id": "codex-runtime-old"},
        )
        registry._peers[peer_id].display_name = "worker"  # noqa: SLF001

    task = asyncio.create_task(register_after_resume_spawn())
    try:
        result = await runner.run_job(child.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert spawn.calls[0]["resume_plan"].runtime_session_id == "codex-runtime-old"
    attempt = result.provenance["runner"]["attempts"][0]
    assert attempt["phase"] == "delivered"
    assert attempt["resume_plan"]["runtime_session_id"] == "codex-runtime-old"
    updated_entry = calendar.get(entry.calendar_id)
    assert updated_entry.provenance["runtime_binding"]["peer_id"] == resumed["peer_id"]
    assert delivery.calls[0]["to_peer"] == resumed["peer_id"]
    assert len(store.list_all()) == 1
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_per_fire_terminal_update_releases_spawned_executor(
    tmp_path,
    monkeypatch,
):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    work = store.create(
        title="daily brief",
        circle="default",
        request={
            "execution": {
                "process_scope": "per_fire",
                "continuity": "fresh",
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    killed: list[str] = []
    monkeypatch.setattr(
        "repowire.daemon.session_control.kill_pane",
        lambda pane_id: killed.append(pane_id) or True,
    )
    monkeypatch.setattr("repowire.daemon.session_control.forget_spawn_ownership", Mock())
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-spawned",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-session-2"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        _session_bindings.upsert_observation(
            peer_id=peer_id,
            backend="codex",
            project_path=worker_path,
            runtime_session_id="codex-session-2",
            resume_capability={
                "supported": True,
                "strategy": "codex_resume",
                "runtime_session_id_arg": "codex-session-2",
            },
            status="active",
            metadata={"hook_session_id": "codex-session-2"},
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        delivered = await runner.run_job(work.work_id)
    finally:
        await task

    attempt_id = delivered.provenance["runner"]["current_attempt_id"]
    assert delivered.provenance["runner"]["attempts"][0]["acquisition"]["release_handle"][
        "pane_id"
    ] == spawn.pane_id

    app = FastAPI()
    app.include_router(work_routes.router)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.patch(
            f"/jobs/{work.work_id}",
            json={
                "state": "completed",
                "attempt_id": attempt_id,
                "result_summary": "done",
            },
        )

    assert response.status_code == 200, response.text
    status = response.json()["status"]
    assert status["state"] == "completed"
    assert killed == [spawn.pane_id]
    release = status["provenance"]["runner"]["release_result"]
    assert release["status"] == "released"
    assert release["pane_id"] == spawn.pane_id
    assert await registry.get_peer(registered["peer_id"]) is None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_per_fire_completion_releases_spawned_executor(tmp_path, monkeypatch):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "moltbook-driver")
    work = store.create(
        title="moltbook sync",
        circle="default",
        request={
            "execution": {
                "process_scope": "per_fire",
                "continuity": "fresh",
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    killed: list[str] = []
    monkeypatch.setattr(
        "repowire.daemon.session_control.kill_pane",
        lambda pane_id: killed.append(pane_id) or True,
    )
    monkeypatch.setattr("repowire.daemon.session_control.forget_spawn_ownership", Mock())
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-moltbook-fire",
            pane_id=spawn.pane_id,
            tmux_session="default:moltbook-driver",
            metadata={"hook_session_id": "codex-session-moltbook"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        delivered = await runner.run_job(work.work_id)
    finally:
        await task

    attempt = delivered.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "spawned_peer"
    assert attempt["acquisition"]["release_handle"]["pane_id"] == spawn.pane_id

    completion = JobCompletionService(
        work_store=store,
        session_control=runner._session_control,  # noqa: SLF001
    )
    await completion.on_chat_turn(
        peer_id=registered["peer_id"],
        role="user",
        text=delivery.calls[0]["text"],
    )
    await completion.on_chat_turn(
        peer_id=registered["peer_id"],
        role="assistant",
        text="Moltbook sync posted.",
    )

    completed = store.get(work.work_id)
    assert completed.state == "completed"
    assert killed == [spawn.pane_id]
    release = completed.provenance["runner"]["release_result"]
    assert release["status"] == "released"
    assert release["peer_id"] == registered["peer_id"]
    assert await registry.get_peer(registered["peer_id"]) is None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_per_fire_cancel_releases_spawned_executor(tmp_path, monkeypatch):
    _cfg, registry, db, store, _calendar, _session_bindings, _delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "moltbook-driver")
    work = store.create(
        title="moltbook sync",
        circle="default",
        request={
            "execution": {
                "process_scope": "per_fire",
                "continuity": "fresh",
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    killed: list[str] = []
    monkeypatch.setattr(
        "repowire.daemon.session_control.probe_tmux_pane",
        lambda pane_id: object() if pane_id == spawn.pane_id else None,
    )
    monkeypatch.setattr(
        "repowire.daemon.session_control.kill_pane",
        lambda pane_id: killed.append(pane_id) or True,
    )
    monkeypatch.setattr("repowire.daemon.session_control.forget_spawn_ownership", Mock())
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-moltbook-cancel",
            pane_id=spawn.pane_id,
            tmux_session="default:moltbook-driver",
            metadata={"hook_session_id": "codex-session-moltbook"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        delivered = await runner.run_job(work.work_id)
    finally:
        await task

    attempt = delivered.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "spawned_peer"
    assert attempt["acquisition"]["release_handle"]["pane_id"] == spawn.pane_id

    app = FastAPI()
    app.include_router(work_routes.router)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post(
            f"/jobs/{work.work_id}/cancel",
            json={"reason": "not_needed"},
        )

    assert response.status_code == 200, response.text
    status = response.json()["status"]
    assert status["state"] == "cancelled"
    assert killed == [spawn.pane_id]
    release = status["provenance"]["runner"]["release_result"]
    assert release["status"] == "released"
    assert release["pane_id"] == spawn.pane_id
    assert await registry.get_peer(registered["peer_id"]) is None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_persistent_completion_keeps_reused_executor_alive(tmp_path, monkeypatch):
    _cfg, registry, db, store, _calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "moltbook-driver")
    peer_id = await _register_peer(
        registry,
        peer_id="repow-default-moltbook-persistent",
        backend=AgentType.CODEX,
        path=worker_path,
        pane_id="%42",
        tmux_session="default:moltbook-driver",
        metadata={"hook_session_id": "codex-session-persistent"},
        status=PeerStatus.ONLINE,
    )
    work = store.create(
        title="moltbook sync",
        circle="default",
        request={
            "execution": {
                "process_scope": "persistent",
                "target": {"path": worker_path, "backend": "codex"},
                "delivery": {"kind": "ask"},
            }
        },
    )
    killed: list[str] = []
    monkeypatch.setattr(
        "repowire.daemon.session_control.kill_pane",
        lambda pane_id: killed.append(pane_id) or True,
    )

    delivered = await runner.run_job(work.work_id)

    assert delivered.state == "delivered"
    assert spawn.calls == []
    attempt = delivered.provenance["runner"]["attempts"][0]
    assert attempt["acquisition"]["strategy"] == "reused_peer"
    assert attempt["acquisition"]["release_handle"] is None

    completion = JobCompletionService(
        work_store=store,
        session_control=runner._session_control,  # noqa: SLF001
    )
    await completion.on_chat_turn(peer_id=peer_id, role="user", text=delivery.calls[0]["text"])
    await completion.on_chat_turn(peer_id=peer_id, role="assistant", text="Still resident.")

    completed = store.get(work.work_id)
    assert completed.state == "completed"
    assert killed == []
    assert "release_result" not in completed.provenance["runner"]
    assert await registry.get_peer(peer_id) is not None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_per_fire_cancel_leaves_assigned_executor_alive(tmp_path, monkeypatch):
    _cfg, registry, db, store, _calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
    peer_id = await _register_peer(
        registry,
        peer_id="repow-default-worker",
        pane_id="%42",
        tmux_session="default:worker",
    )
    work = store.create(
        title="cancel",
        assigned_peer_id=peer_id,
        circle="default",
        request={
            "execution": {
                "process_scope": "per_fire",
                "target": {"assigned_peer_id": peer_id},
            }
        },
    )
    killed: list[str] = []
    monkeypatch.setattr(
        "repowire.daemon.session_control.probe_tmux_pane",
        lambda pane_id: object() if pane_id == "%42" else None,
    )
    monkeypatch.setattr(
        "repowire.daemon.session_control.kill_pane",
        lambda pane_id: killed.append(pane_id) or True,
    )
    monkeypatch.setattr("repowire.daemon.session_control.forget_spawn_ownership", Mock())

    delivered = await runner.run_job(work.work_id)
    assert delivered.state == "delivered"
    assert delivered.provenance["runner"]["attempts"][0]["acquisition"]["release_handle"] is None

    app = FastAPI()
    app.include_router(work_routes.router)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post(
            f"/jobs/{work.work_id}/cancel",
            json={"reason": "not_needed"},
        )

    assert response.status_code == 200, response.text
    status = response.json()["status"]
    assert status["state"] == "cancelled"
    assert killed == []
    assert "release_result" not in status["provenance"]["runner"]
    assert await registry.get_peer(peer_id) is not None
    db.close()
    cleanup_deps()


@pytest.mark.anyio
async def test_per_fire_fresh_ignores_recorded_resume_binding(tmp_path):
    _cfg, registry, db, store, calendar, _session_bindings, delivery, spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    calendar.create(
        title="daily brief",
        kind="brief",
        cron="*/2 * * * *",
        circle="default",
        request={
            "execution": {
                "process_scope": "per_fire",
                "continuity": "fresh",
                "target": {"path": worker_path, "backend": "codex"},
            }
        },
        provenance={
            "runtime_binding": {
                "peer_id": "repow-default-old",
                "backend": "codex",
                "path": worker_path,
                "circle": "default",
                "runtime_session_id": "codex-runtime-old",
                "resume_capability": {
                    "supported": True,
                    "strategy": "codex_resume",
                    "runtime_session_id_arg": "codex-runtime-old",
                },
            }
        },
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )
    [child] = calendar.materialize_due(now=datetime(2026, 5, 25, 8, 2, tzinfo=timezone.utc))
    registered: dict[str, str] = {}

    async def register_after_spawn() -> None:
        while not spawn.calls:
            await asyncio.sleep(0.01)
        peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path=worker_path,
            peer_id="repow-default-fresh",
            pane_id=spawn.pane_id,
            tmux_session="default:daily-email-brief",
            metadata={"hook_session_id": "codex-runtime-new"},
            initial_status=PeerStatus.ONLINE,
            role=PeerRole.AGENT,
        )
        registered["peer_id"] = peer_id

    task = asyncio.create_task(register_after_spawn())
    try:
        result = await runner.run_job(child.work_id)
    finally:
        await task

    assert result.state == "delivered"
    assert spawn.calls[0]["resume_plan"] is None
    assert delivery.calls[0]["to_peer"] == registered["peer_id"]
    db.close()
    cleanup_deps()


def test_recurring_job_does_not_resume_when_binding_marks_resume_unsupported(tmp_path):
    _cfg, _registry, db, store, calendar, _session_bindings, _delivery, _spawn, runner = _env(
        tmp_path
    )
    worker_path = str(tmp_path / "daily-email-brief")
    calendar.create(
        title="daily brief",
        kind="brief",
        cron="*/2 * * * *",
        circle="default",
        request={"execution": {"target": {"path": worker_path, "backend": "codex"}}},
        provenance={
            "runtime_binding": {
                "peer_id": "repow-default-old",
                "backend": "codex",
                "path": worker_path,
                "circle": "default",
                "runtime_session_id": "codex-runtime-old",
                "resume_capability": {
                    "supported": False,
                    "strategy": "codex_resume",
                    "reason": "runtime_marked_unresumable",
                },
            }
        },
        now=datetime(2026, 5, 25, 8, 0, tzinfo=timezone.utc),
    )
    [child] = calendar.materialize_due(now=datetime(2026, 5, 25, 8, 2, tzinfo=timezone.utc))

    assert runner._resume_plan_for(child, path=worker_path, backend=AgentType.CODEX) is None
    assert (
        runner._session_control._resume_plan_for(
            child,
            path=worker_path,
            backend=AgentType.CODEX,
        )
        is None
    )
    db.close()
    cleanup_deps()


def _parse_iso_for_test(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def test_spawn_service_builds_codex_resume_command() -> None:
    command = SpawnService.resume_command(
        "codex --dangerously-bypass-approvals-and-sandbox",
        backend=AgentType.CODEX,
        resume_plan=AgentResumePlan(
            backend=AgentType.CODEX,
            runtime_session_id="codex-runtime-1",
            capability={"supported": True, "strategy": "codex_resume"},
        ),
    )

    assert command == "codex --dangerously-bypass-approvals-and-sandbox resume codex-runtime-1"


def test_agent_backend_registry_covers_every_agent_type() -> None:
    assert set(AGENT_BACKENDS) == set(AgentType)
    for backend_type, backend in AGENT_BACKENDS.items():
        assert backend.agent_type == backend_type
        if backend.supports_local_spawn:
            assert backend.default_command
            assert DEFAULT_SPAWN_COMMANDS[backend_type] == backend.default_command


def test_agent_backends_make_codex_resume_first_class() -> None:
    capability = resume_capability_for_registration(AgentType.CODEX, "runtime-123")

    assert capability == {
        "supported": True,
        "strategy": "codex_resume",
        "runtime_session_id_arg": "runtime-123",
    }
    assert can_resume_backend(AgentType.CODEX, runtime_session_id="runtime-123")
    assert not can_resume_backend(AgentType.CODEX, runtime_session_id=None)


@pytest.mark.parametrize(
    ("backend", "base_command", "expected"),
    [
        (
            AgentType.CLAUDE_CODE,
            "claude --dangerously-skip-permissions",
            "claude --dangerously-skip-permissions --resume runtime-123",
        ),
        (
            AgentType.GEMINI,
            "gemini --yolo",
            "gemini --yolo --resume runtime-123",
        ),
        (
            AgentType.OPENCODE,
            "opencode",
            "opencode --session runtime-123",
        ),
        (
            AgentType.ANTIGRAVITY,
            "agy --dangerously-skip-permissions",
            "agy --dangerously-skip-permissions --conversation runtime-123",
        ),
        (
            AgentType.PI,
            "pi",
            "pi --session runtime-123",
        ),
    ],
)
def test_agent_backends_build_resume_commands_for_cli_backends(
    backend: AgentType,
    base_command: str,
    expected: str,
) -> None:
    capability = resume_capability_for_registration(backend, "runtime-123")

    assert capability["supported"] is True
    assert can_resume_backend(backend, runtime_session_id="runtime-123")
    assert (
        SpawnService.resume_command(
            base_command,
            backend=backend,
            resume_plan=AgentResumePlan(
                backend=backend,
                runtime_session_id="runtime-123",
                capability=capability,
            ),
        )
        == expected
    )


def test_agent_backends_cover_unsupported_mcp_http_explicitly() -> None:
    capability = resume_capability_for_registration(AgentType.MCP_HTTP, "runtime-123")

    assert capability["supported"] is False
    assert capability["reason"] == "backend_resume_not_implemented"
    assert not can_resume_backend(AgentType.MCP_HTTP, runtime_session_id="runtime-123")


@pytest.mark.anyio
async def test_spawn_service_passes_codex_resume_command_to_spawn(tmp_path, monkeypatch):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CODEX] = "codex --dangerously-bypass-approvals-and-sandbox"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    tasks: set[asyncio.Task] = set()
    spawn_impl = Mock(
        return_value=SimpleNamespace(
            display_name="daily-email-brief",
            tmux_session="default:daily-email-brief",
            pane_id="%661",
            message="warm",
        )
    )
    monkeypatch.setattr("repowire.daemon.spawn_service.record_spawn_ownership", Mock())

    async def warmup(*_args, **_kwargs):
        return None

    service = SpawnService(
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=set(),
        background_tasks=tasks,
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    service.spawn(
        path=str(tmp_path),
        backend=AgentType.CODEX,
        message="warm",
        resume_plan=AgentResumePlan(
            backend=AgentType.CODEX,
            runtime_session_id="codex-runtime-1",
            capability={"supported": True, "strategy": "codex_resume"},
        ),
    )

    spawn_config = spawn_impl.call_args.args[0]
    assert spawn_config.command == (
        "codex --dangerously-bypass-approvals-and-sandbox resume codex-runtime-1"
    )


@pytest.mark.anyio
async def test_spawn_service_uses_explicit_env_path(tmp_path, monkeypatch):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CODEX] = "codex"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    cfg.daemon.spawn.env_path = ["~/.local/bin", "/opt/homebrew/bin"]
    cfg.daemon.spawn.env = {"GWS_CONFIG": "~/calendar.yaml"}
    spawn_impl = Mock(
        return_value=SimpleNamespace(
            display_name="daily-email-brief",
            tmux_session="default:daily-email-brief",
            pane_id="%661",
            message="warm",
        )
    )
    monkeypatch.setattr("repowire.daemon.spawn_service.capture_login_shell_path", Mock())
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.shutil.which",
        Mock(return_value="/opt/homebrew/bin/codex"),
    )
    monkeypatch.setattr("repowire.daemon.spawn_service.record_spawn_ownership", Mock())

    async def warmup(*_args, **_kwargs):
        return None

    service = SpawnService(
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=set(),
        background_tasks=set(),
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    result = service.spawn(path=str(tmp_path), backend=AgentType.CODEX, message="warm")

    spawn_config = spawn_impl.call_args.args[0]
    assert spawn_config.env == {
        "GWS_CONFIG": "~/calendar.yaml",
        "PATH": f"{Path('~/.local/bin').expanduser()}{os.pathsep}/opt/homebrew/bin",
    }
    assert result.env == spawn_config.env


@pytest.mark.anyio
async def test_spawn_service_falls_back_to_login_shell_path(tmp_path, monkeypatch):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CODEX] = "codex"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    spawn_impl = Mock(
        return_value=SimpleNamespace(
            display_name="daily-email-brief",
            tmux_session="default:daily-email-brief",
            pane_id="%661",
            message="warm",
        )
    )
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.capture_login_shell_path",
        Mock(return_value="/nvm/bin:/opt/homebrew/bin:/usr/bin"),
    )
    monkeypatch.setattr("repowire.daemon.spawn_service.record_spawn_ownership", Mock())

    async def warmup(*_args, **_kwargs):
        return None

    service = SpawnService(
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=set(),
        background_tasks=set(),
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    service.spawn(path=str(tmp_path), backend=AgentType.CODEX, message="warm")

    spawn_config = spawn_impl.call_args.args[0]
    assert spawn_config.env == {"PATH": "/nvm/bin:/opt/homebrew/bin:/usr/bin"}


@pytest.mark.anyio
async def test_spawn_service_explicit_env_path_fails_fast_when_command_missing(
    tmp_path, monkeypatch,
):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CODEX] = "codex"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    cfg.daemon.spawn.env_path = ["/nope"]
    spawn_impl = Mock()
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.shutil.which",
        Mock(return_value=None),
    )

    service = SpawnService(
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=set(),
        background_tasks=set(),
        spawn_impl=spawn_impl,
    )

    with pytest.raises(Exception) as exc_info:
        service.spawn(path=str(tmp_path), backend=AgentType.CODEX, message="warm")

    assert getattr(exc_info.value, "status_code") == 422
    assert exc_info.value.detail["error"] == "command_unavailable"
    spawn_impl.assert_not_called()


def test_capture_login_shell_path_ignores_noisy_startup_output(monkeypatch):
    def fake_run(*_args, **_kwargs):
        return SimpleNamespace(
            returncode=0,
            stdout=(
                "hello from zshrc\n"
                "REPOWIRE_PATH_START:/nvm/bin:/opt/homebrew/bin:/usr/bin:"
                "REPOWIRE_PATH_END\n"
                "goodbye from zshrc\n"
            ),
        )

    monkeypatch.setattr("repowire.daemon.spawn_service.subprocess.run", fake_run)

    assert capture_login_shell_path() == "/nvm/bin:/opt/homebrew/bin:/usr/bin"


def test_capture_login_shell_path_requires_sentinel(monkeypatch):
    def fake_run(*_args, **_kwargs):
        return SimpleNamespace(returncode=0, stdout="hello\n/usr/bin:/bin\n")

    monkeypatch.setattr("repowire.daemon.spawn_service.subprocess.run", fake_run)

    assert capture_login_shell_path() is None


def test_command_head_for_probe_uses_last_and_segment():
    assert command_head_for_probe("cd /work && codex --model fast") == "codex"


@pytest.mark.anyio
async def test_spawn_service_registration_retry_captures_both_attempts_and_cleans_up(
    tmp_path, monkeypatch,
):
    cfg = Config()
    cfg.daemon.spawn.commands[AgentType.CODEX] = "codex"
    cfg.daemon.spawn.allowed_paths = [str(tmp_path)]
    spawned = [
        SimpleNamespace(
            display_name="daily-email-brief",
            tmux_session="default:daily-email-brief",
            pane_id="%701",
            message="warm",
        ),
        SimpleNamespace(
            display_name="daily-email-brief-2",
            tmux_session="default:daily-email-brief-2",
            pane_id="%702",
            message="warm",
        ),
    ]
    spawn_impl = Mock(side_effect=spawned)
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.capture_login_shell_path",
        Mock(return_value="/usr/bin:/bin"),
    )
    monkeypatch.setattr("repowire.daemon.spawn_service.record_spawn_ownership", Mock())
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.capture_spawn_registration_diagnostics",
        AsyncMock(side_effect=[
            {"pane_id": "%701", "pane_live": True},
            {"pane_id": "%702", "pane_live": False},
        ]),
    )
    kill_pane = Mock(return_value=True)
    forget_spawn_ownership = Mock()
    monkeypatch.setattr("repowire.daemon.spawn_service.kill_pane", kill_pane)
    monkeypatch.setattr(
        "repowire.daemon.spawn_service.forget_spawn_ownership",
        forget_spawn_ownership,
    )

    async def never_registered(_spawn_result):
        return None

    async def warmup(*_args, **_kwargs):
        return None

    service = SpawnService(
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=set(),
        background_tasks=set(),
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    result = await service.spawn_registered(
        path=str(tmp_path),
        backend=AgentType.CODEX,
        message="warm",
        await_peer=never_registered,
    )

    assert result.resolved_peer is None
    assert [item["attempt"] for item in result.registration_attempts] == [1, 2]
    assert [item["pane_id"] for item in result.registration_attempts] == ["%701", "%702"]
    assert kill_pane.call_args_list[0].args == ("%701",)
    assert kill_pane.call_args_list[1].args == ("%702",)
    assert forget_spawn_ownership.call_args_list[0].args == ("%701",)
    assert forget_spawn_ownership.call_args_list[1].args == ("%702",)


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
        spawn=cfg.daemon.spawn,
        spawned_pane_ids=spawned,
        background_tasks=tasks,
        spawn_impl=spawn_impl,
        warmup_impl=warmup,
    )

    result = service.spawn(path=str(tmp_path), backend=AgentType.CLAUDE_CODE, message="warm")

    assert result.pane_id == "%1"
    assert "%1" in spawned
    assert tasks
