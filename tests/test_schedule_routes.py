"""Tests for /schedules HTTP routes."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import schedules
from repowire.daemon.schedule_store import ScheduleStore
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_app(tmp_path: Path):
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

    store = ScheduleStore(tmp_path / "schedules.json")
    # Mock scheduler — routes only need .notify_changed()
    scheduler = MagicMock()
    scheduler.notify_changed = MagicMock()

    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        schedule_store=store,
        scheduler=scheduler,
        relay_mode=False,
    )
    init_deps(cfg, registry, state)

    app = FastAPI()
    app.include_router(schedules.router)
    return app, store, scheduler


@pytest.fixture
async def env(tmp_path):
    app, store, scheduler = _make_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c, store, scheduler
    cleanup_deps()


def _future_iso(seconds: float = 60.0) -> str:
    return (datetime.now(timezone.utc) + timedelta(seconds=seconds)).isoformat()


class TestCreate:
    async def test_returns_schedule_id_and_wakes_scheduler(self, env):
        client, store, scheduler = env
        r = await client.post("/schedules", json={
            "from_peer": "alice",
            "to_peer": "bob",
            "text": "ping",
            "fire_at": _future_iso(),
        })
        assert r.status_code == 200
        body = r.json()
        assert body["schedule_id"].startswith("sched-")
        assert body["kind"] == "notify"
        assert store.get(body["schedule_id"]) is not None
        scheduler.notify_changed.assert_called_once()

    async def test_rejects_invalid_fire_at(self, env):
        client, _, _ = env
        r = await client.post("/schedules", json={
            "from_peer": "alice", "to_peer": "bob",
            "text": "x", "fire_at": "not-a-date",
        })
        assert r.status_code == 400

    async def test_rejects_unknown_kind(self, env):
        client, _, _ = env
        r = await client.post("/schedules", json={
            "from_peer": "alice", "to_peer": "bob",
            "text": "x", "fire_at": _future_iso(), "kind": "cron",
        })
        assert r.status_code == 400


class TestList:
    async def test_empty(self, env):
        client, _, _ = env
        r = await client.get("/schedules")
        assert r.status_code == 200
        assert r.json()["schedules"] == []

    async def test_filter_by_from_peer(self, env):
        client, _, _ = env
        await client.post("/schedules", json={
            "from_peer": "alice", "to_peer": "bob",
            "text": "a", "fire_at": _future_iso(60),
        })
        await client.post("/schedules", json={
            "from_peer": "eve", "to_peer": "bob",
            "text": "e", "fire_at": _future_iso(30),
        })
        r = await client.get("/schedules", params={"from_peer": "alice"})
        items = r.json()["schedules"]
        assert len(items) == 1
        assert items[0]["from_peer"] == "alice"


class TestDelete:
    async def test_removes_and_wakes(self, env):
        client, store, scheduler = env
        r = await client.post("/schedules", json={
            "from_peer": "alice", "to_peer": "bob",
            "text": "x", "fire_at": _future_iso(),
        })
        sid = r.json()["schedule_id"]
        scheduler.notify_changed.reset_mock()

        r = await client.delete(f"/schedules/{sid}")
        assert r.status_code == 200
        assert store.get(sid) is None
        scheduler.notify_changed.assert_called_once()

    async def test_unknown_returns_404(self, env):
        client, _, _ = env
        r = await client.delete("/schedules/sched-nope")
        assert r.status_code == 404
