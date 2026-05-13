"""Tests for orchestrator liveness: is_orchestrator_present + /circles/{name}/orchestrator."""

from __future__ import annotations

import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerRole, PeerStatus


def _make_registry(tmp_path: Path, heartbeat: int = 30) -> PeerRegistry:
    cfg = Config()
    cfg.daemon.heartbeat_interval = heartbeat
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    return registry


async def _register_orch(
    registry: PeerRegistry,
    *,
    peer_id: str = "repow-team-orch1",
    name: str = "orch",
    circle: str = "team",
    status: PeerStatus = PeerStatus.ONLINE,
    last_seen: datetime | None = None,
) -> Peer:
    peer = Peer(
        peer_id=peer_id,
        display_name=name,
        path=f"/tmp/{name}",
        machine="m",
        circle=circle,
        backend=AgentType.CLAUDE_CODE,
        role=PeerRole.ORCHESTRATOR,
        status=status,
        last_seen=last_seen or datetime.now(timezone.utc),
    )
    await registry.register_peer(peer)
    # register_peer forces status=ONLINE + bumps last_seen; restore caller's intent
    async with registry._lock:
        live = registry._peers[peer.peer_id]
        live.status = status
        if last_seen is not None:
            live.last_seen = last_seen
    return peer


class TestHeartbeatTolerance:
    def test_returns_double_heartbeat_interval(self, tmp_path):
        registry = _make_registry(tmp_path, heartbeat=30)
        assert registry.heartbeat_tolerance() == 60

    def test_tracks_config_change(self, tmp_path):
        registry = _make_registry(tmp_path, heartbeat=5)
        assert registry.heartbeat_tolerance() == 10


class TestIsOrchestratorPresent:
    async def test_returns_false_when_no_orchestrator(self, tmp_path):
        registry = _make_registry(tmp_path)
        assert await registry.is_orchestrator_present("team") is False

    async def test_returns_true_for_fresh_orchestrator(self, tmp_path):
        registry = _make_registry(tmp_path)
        await _register_orch(registry, circle="team")
        assert await registry.is_orchestrator_present("team") is True

    async def test_scoped_per_circle(self, tmp_path):
        registry = _make_registry(tmp_path)
        await _register_orch(registry, circle="team", peer_id="o1", name="o1")
        assert await registry.is_orchestrator_present("team") is True
        assert await registry.is_orchestrator_present("other") is False

    async def test_offline_orchestrator_is_not_present(self, tmp_path):
        registry = _make_registry(tmp_path)
        await _register_orch(
            registry, circle="team", status=PeerStatus.OFFLINE,
        )
        assert await registry.is_orchestrator_present("team") is False

    async def test_stale_heartbeat_flips_present_false(self, tmp_path):
        """The headline scenario: kill heartbeat, present flips false past 2x interval."""
        registry = _make_registry(tmp_path, heartbeat=30)
        fresh = datetime.now(timezone.utc) - timedelta(seconds=10)
        await _register_orch(registry, circle="team", last_seen=fresh)
        assert await registry.is_orchestrator_present("team") is True

        # Simulate two missed heartbeats (>60s old).
        stale = datetime.now(timezone.utc) - timedelta(seconds=75)
        async with registry._lock:
            registry._peers["repow-team-orch1"].last_seen = stale

        assert await registry.is_orchestrator_present("team") is False

    async def test_one_missed_beat_still_present(self, tmp_path):
        """Within 2x interval (one missed beat tolerated) the orch is still present."""
        registry = _make_registry(tmp_path, heartbeat=30)
        await _register_orch(registry, circle="team")
        # 45s old: more than one interval but less than two.
        async with registry._lock:
            registry._peers["repow-team-orch1"].last_seen = (
                datetime.now(timezone.utc) - timedelta(seconds=45)
            )
        assert await registry.is_orchestrator_present("team") is True

    async def test_agent_role_does_not_count(self, tmp_path):
        registry = _make_registry(tmp_path)
        peer = Peer(
            peer_id="repow-team-agent1",
            display_name="agent",
            path="/tmp/agent",
            machine="m",
            circle="team",
            backend=AgentType.CLAUDE_CODE,
            role=PeerRole.AGENT,
            status=PeerStatus.ONLINE,
            last_seen=datetime.now(timezone.utc),
        )
        await registry.register_peer(peer)
        assert await registry.is_orchestrator_present("team") is False

    async def test_picks_most_recent_when_multiple(self, tmp_path):
        registry = _make_registry(tmp_path)
        older = datetime.now(timezone.utc) - timedelta(seconds=20)
        newer = datetime.now(timezone.utc) - timedelta(seconds=2)
        await _register_orch(
            registry, peer_id="o-old", name="o-old", circle="team", last_seen=older,
        )
        await _register_orch(
            registry, peer_id="o-new", name="o-new", circle="team", last_seen=newer,
        )
        winner = await registry.get_orchestrator("team")
        assert winner is not None
        assert winner.peer_id == "o-new"


# -- HTTP route --


def _make_app(tmp_path: Path, heartbeat: int = 30) -> tuple[FastAPI, PeerRegistry]:
    cfg = Config()
    cfg.daemon.heartbeat_interval = heartbeat
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state)

    app = FastAPI()
    app.include_router(peers_routes.router)
    return app, registry


@pytest.fixture
async def app_and_registry(tmp_path):
    app, registry = _make_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as client:
        yield client, registry
    cleanup_deps()


class TestCircleOrchestratorRoute:
    async def test_absent(self, app_and_registry):
        client, _ = app_and_registry
        r = await client.get("/circles/team/orchestrator")
        assert r.status_code == 200
        body = r.json()
        assert body == {
            "circle": "team",
            "present": False,
            "peer_id": None,
            "peer_name": None,
            "last_seen": None,
            "stale_after_seconds": 60,
        }

    async def test_present(self, app_and_registry):
        client, registry = app_and_registry
        await _register_orch(registry, circle="team", name="orch", peer_id="orch-1")
        r = await client.get("/circles/team/orchestrator")
        assert r.status_code == 200
        body = r.json()
        assert body["present"] is True
        assert body["circle"] == "team"
        assert body["peer_name"] == "orch"
        assert body["peer_id"] == "orch-1"
        assert body["last_seen"] is not None
        assert body["stale_after_seconds"] == 60

    async def test_flips_to_absent_when_heartbeat_dies(self, app_and_registry):
        """Headline acceptance test: kill heartbeat, route flips present→false."""
        client, registry = app_and_registry
        await _register_orch(registry, circle="team", name="orch", peer_id="orch-1")
        r = await client.get("/circles/team/orchestrator")
        assert r.json()["present"] is True

        # Simulate the orchestrator's heartbeat stopping >2x interval ago.
        async with registry._lock:
            registry._peers["orch-1"].last_seen = (
                datetime.now(timezone.utc) - timedelta(seconds=90)
            )

        r = await client.get("/circles/team/orchestrator")
        assert r.status_code == 200
        body = r.json()
        assert body["present"] is False
        assert body["peer_id"] is None
