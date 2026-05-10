"""Tests for /ask, /ack, and /asks/* HTTP routes."""

import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import asks, peers
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
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, state)

    # Stub the wire-level send so /ask and /ack don't fail on missing transport
    router.send_notification = AsyncMock()
    router.send_ask = AsyncMock()

    app = FastAPI()
    app.include_router(peers.router)
    app.include_router(asks.router)
    return app, registry, at


@pytest.fixture
async def env(tmp_path):
    app, registry, at = _make_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c, registry, at
    cleanup_deps()


async def _register_peer(client, name: str, pane_id: str | None = None) -> str:
    body: dict = {
        "name": name,
        "path": f"/tmp/{name}",
        "circle": "default",
        "backend": "claude-code",
    }
    if pane_id:
        body["pane_id"] = pane_id
    r = await client.post("/peers", json=body)
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


class TestAsk:
    async def test_returns_correlation_id(self, env):
        client, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice",
            "to_peer": bob,
            "text": "what's up?",
        })
        assert r.status_code == 200
        body = r.json()
        assert body["correlation_id"].startswith("ask-")
        assert body["status"] in ("sent", "queued")

    async def test_unknown_peer_returns_404(self, env):
        client, _, _ = env
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": "ghost", "text": "?",
        })
        assert r.status_code == 404

    async def test_reply_to_closes_prior(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "first",
        })
        first_cid = r.json()["correlation_id"]

        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "follow-up",
            "reply_to": first_cid,
        })
        assert r.status_code == 200
        prior = await at.get(first_cid)
        assert prior.closed
        assert prior.close_reason == "reply_to"


class TestAck:
    async def test_bare_ack_closes(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack"

    async def test_ack_with_msg_delivers_reply(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "status?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob, "message": "all good",
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack_with_msg"

    async def test_ack_unknown_id_404(self, env):
        client, _, _ = env
        r = await client.post("/ack", json={
            "correlation_id": "ask-never", "from_peer": "x",
        })
        assert r.status_code == 404

    async def test_ack_idempotent(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        r2 = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        # Idempotent: second ack returns 200 not 404
        assert r2.status_code == 200


class TestPickedUp:
    async def test_records_pickup(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        # Body no longer carries turn_seq; daemon snapshots its own counter.
        r = await client.post(f"/asks/{cid}/picked_up", json={
            "correlation_id": cid,
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.picked_up
        assert ask.picked_up_turn_seq == 0  # no /asks/pending called yet


class TestPendingAsks:
    async def test_grace_window(self, env):
        client, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%50")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        # Transport-style pickup at delivery: snapshots turn_seq=0 (no
        # /asks/pending has been called yet).
        await client.post(f"/asks/{cid}/picked_up", json={"correlation_id": cid})

        # First /asks/pending bumps counter to 1. picked_up_turn_seq=0 < 1 true.
        r = await client.get("/asks/pending?pane_id=%2550")
        assert r.status_code == 200
        body = r.json()
        assert body["current_turn_seq"] == 1
        assert len(body["asks"]) == 1
        assert body["asks"][0]["correlation_id"] == cid

    async def test_pickup_after_pending_not_flagged_same_cycle(self, env):
        """Race case: /asks/pending bumps before pickup → same cycle skips."""
        client, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%51")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        # Stop hook fires first: /asks/pending bumps to 1. Ask not picked up
        # yet → empty result.
        r = await client.get("/asks/pending?pane_id=%2551")
        assert r.json()["asks"] == []
        # Pickup arrives next: snapshots seq=1.
        await client.post(f"/asks/{cid}/picked_up", json={"correlation_id": cid})
        # Next Stop fire bumps to 2 → flagged.
        r = await client.get("/asks/pending?pane_id=%2551")
        body = r.json()
        assert body["current_turn_seq"] == 2
        assert len(body["asks"]) == 1

    async def test_unknown_pane(self, env):
        client, _, _ = env
        r = await client.get("/asks/pending?pane_id=%25nope")
        assert r.status_code == 404


class TestMarkReminded:
    async def test_flips_flag(self, env):
        client, _, at = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        r = await client.post(f"/asks/{cid}/mark_reminded", json={
            "correlation_id": cid,
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.reminded
