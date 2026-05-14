"""Tests for turn_state field on Peer + plumbing through registry, route, hints."""

from __future__ import annotations

import time
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
from repowire.daemon.routes import messages, peers
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerStatus
from repowire.spawn_hints import consume_hint_full, write_hint

# ---------- protocol model ----------

def test_peer_default_turn_state_is_none() -> None:
    p = Peer(peer_id="repow-x-aaaa", display_name="foo", path="/x", machine="m")
    assert p.turn_state is None


def test_peer_to_dict_includes_turn_state() -> None:
    p = Peer(
        peer_id="repow-x-aaaa", display_name="foo", path="/x", machine="m",
        turn_state="awaiting_input",
    )
    assert p.to_dict()["turn_state"] == "awaiting_input"


# ---------- spawn_hints ----------

@pytest.fixture
def tmp_cache(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    monkeypatch.setattr("repowire.spawn_hints.CACHE_DIR", tmp_path)
    return tmp_path


def test_write_hint_pending_first_turn_roundtrips(tmp_cache: Path) -> None:
    write_hint("/tmp/seeded", "claude-code", "default", pending_first_turn=True)
    data = consume_hint_full("/tmp/seeded", "claude-code")
    assert data is not None
    assert data.get("pending_first_turn") is True


def test_write_hint_without_pending_first_turn_omits_field(tmp_cache: Path) -> None:
    write_hint("/tmp/plain", "claude-code", "default")
    data = consume_hint_full("/tmp/plain", "claude-code")
    assert data is not None
    assert "pending_first_turn" not in data


# ---------- peer_registry ----------

def _make_registry(tmp_path: Path) -> PeerRegistry:
    cfg = Config()
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    reg = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    reg._events_path = tmp_path / "events.json"
    reg._events.clear()
    reg._last_repair = time.monotonic() + 3600
    return reg


@pytest.mark.asyncio
async def test_update_peer_turn_state_applies(tmp_path: Path) -> None:
    reg = _make_registry(tmp_path)
    peer_id, _ = await reg.allocate_and_register(
        circle="c", backend=AgentType.CLAUDE_CODE, path="/p",
    )
    await reg.update_peer_turn_state(peer_id, "awaiting_input")
    peer = await reg.get_peer(peer_id)
    assert peer is not None
    assert peer.turn_state == "awaiting_input"

    await reg.update_peer_turn_state(peer_id, None)
    peer = await reg.get_peer(peer_id)
    assert peer is not None
    assert peer.turn_state is None


@pytest.mark.asyncio
async def test_update_peer_turn_state_does_not_change_status(tmp_path: Path) -> None:
    """turn_state is orthogonal to status -- updating one must not move the other."""
    reg = _make_registry(tmp_path)
    peer_id, _ = await reg.allocate_and_register(
        circle="c", backend=AgentType.CLAUDE_CODE, path="/p",
    )
    await reg.update_peer_status(peer_id, PeerStatus.BUSY)
    await reg.update_peer_turn_state(peer_id, "working")
    peer = await reg.get_peer(peer_id)
    assert peer is not None
    assert peer.status == PeerStatus.BUSY
    assert peer.turn_state == "working"


@pytest.mark.asyncio
async def test_allocate_and_register_accepts_initial_turn_state(tmp_path: Path) -> None:
    reg = _make_registry(tmp_path)
    peer_id, _ = await reg.allocate_and_register(
        circle="c",
        backend=AgentType.CLAUDE_CODE,
        path="/p",
        turn_state="pending_first_turn",
    )
    peer = await reg.get_peer(peer_id)
    assert peer is not None
    assert peer.turn_state == "pending_first_turn"


# ---------- HTTP routes ----------

def _make_test_app(tmp_path: Path) -> FastAPI:
    cfg = Config()
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=tracker,
        transport=transport, persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    app_state = SimpleNamespace(
        config=cfg, transport=transport, query_tracker=tracker,
        message_router=router, peer_registry=registry, relay_mode=False,
    )
    init_deps(cfg, registry, app_state)
    app = FastAPI()
    app.include_router(peers.router)
    app.include_router(messages.router)
    return app


@pytest.fixture
async def client(tmp_path: Path):
    app = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c
    cleanup_deps()


@pytest.mark.asyncio
async def test_session_update_accepts_turn_state_alone(client) -> None:
    resp = await client.post("/peers", json={
        "name": "alpha", "path": "/p", "circle": "c", "backend": "claude-code",
    })
    assert resp.status_code == 200
    peer_id = resp.json()["peer_id"]

    resp = await client.post("/session/update", json={
        "peer_name": peer_id, "turn_state": "working",
    })
    assert resp.status_code == 200

    resp = await client.get(f"/peers/{peer_id}")
    assert resp.status_code == 200
    assert resp.json()["turn_state"] == "working"


@pytest.mark.asyncio
async def test_session_update_requires_status_or_turn_state(client) -> None:
    resp = await client.post("/peers", json={
        "name": "beta", "path": "/p2", "circle": "c", "backend": "claude-code",
    })
    peer_id = resp.json()["peer_id"]
    resp = await client.post("/session/update", json={"peer_name": peer_id})
    assert resp.status_code == 400


@pytest.mark.asyncio
async def test_session_update_status_and_turn_state_together(client) -> None:
    resp = await client.post("/peers", json={
        "name": "gamma", "path": "/p3", "circle": "c", "backend": "claude-code",
    })
    peer_id = resp.json()["peer_id"]
    resp = await client.post("/session/update", json={
        "peer_name": peer_id, "status": "busy", "turn_state": "working",
    })
    assert resp.status_code == 200
    body = (await client.get(f"/peers/{peer_id}")).json()
    assert body["status"] == "busy"
    assert body["turn_state"] == "working"


@pytest.mark.asyncio
async def test_register_peer_accepts_initial_turn_state(client) -> None:
    """Session handler can register a spawn-seeded peer as pending_first_turn."""
    resp = await client.post("/peers", json={
        "name": "spawned", "path": "/sp", "circle": "c", "backend": "claude-code",
        "turn_state": "pending_first_turn",
    })
    assert resp.status_code == 200
    peer_id = resp.json()["peer_id"]
    assert (await client.get(f"/peers/{peer_id}")).json()["turn_state"] == "pending_first_turn"
