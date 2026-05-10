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
from repowire.daemon.websocket_transport import TransportError, WebSocketTransport


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
    return app, registry, at, router


@pytest.fixture
async def env(tmp_path):
    app, registry, at, msg_router = _make_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c, registry, at, msg_router
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
        client, _, _, _ = env
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

    async def test_unknown_peer_returns_404(self, env):
        client, _, _, _ = env
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": "ghost", "text": "?",
        })
        assert r.status_code == 404

    async def test_transport_error_returns_503_and_rolls_back(self, env):
        client, _, at, msg_router = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        msg_router.send_ask.side_effect = TransportError("No connection")

        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        assert r.status_code == 503
        # No phantom open ask should remain
        assert at.open_count() == 0

    async def test_reply_to_closes_prior(self, env):
        client, _, at, _ = env
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

    async def test_reply_to_not_closed_when_send_fails(self, env):
        client, _, at, msg_router = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        # First ask succeeds
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "first",
        })
        first_cid = r.json()["correlation_id"]
        prior = await at.get(first_cid)
        assert not prior.closed

        # Second ask with reply_to fails to send → prior must remain open
        msg_router.send_ask.side_effect = TransportError("No connection")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "follow-up",
            "reply_to": first_cid,
        })
        assert r.status_code == 503
        prior = await at.get(first_cid)
        assert not prior.closed


class TestAck:
    async def test_bare_ack_closes(self, env):
        client, _, at, _ = env
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
        client, _, at, _ = env
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
        client, _, _, _ = env
        r = await client.post("/ack", json={
            "correlation_id": "ask-never", "from_peer": "x",
        })
        assert r.status_code == 404

    async def test_ack_with_msg_503_when_reply_undeliverable(self, env):
        """If reply delivery fails (no live WS), ack returns 503 and ask stays open."""
        client, _, at, msg_router = env
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice, "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        # Asker's notify path now fails
        msg_router.send_notification.side_effect = TransportError("No connection")

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob, "message": "all good",
        })
        assert r.status_code == 503
        ask = await at.get(cid)
        assert not ask.closed  # MUST remain open so recipient can retry

    async def test_bare_ack_succeeds_even_if_router_would_fail(self, env):
        """Bare ack doesn't deliver anything, so router state is irrelevant."""
        client, _, at, msg_router = env
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice, "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        msg_router.send_notification.side_effect = TransportError("dead")

        r = await client.post("/ack", json={"correlation_id": cid, "from_peer": bob})
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed

    async def test_ack_idempotent(self, env):
        client, _, _, _ = env
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
        assert r2.status_code == 200


class TestPendingAsks:
    async def test_returns_open_asks(self, env):
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%50")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.get("/asks/pending?pane_id=%2550")
        assert r.status_code == 200
        body = r.json()
        assert len(body["asks"]) == 1
        assert body["asks"][0]["correlation_id"] == cid
        # Slim shape — no current_turn_seq, no picked_up_at
        assert "current_turn_seq" not in body
        assert "picked_up_at" not in body["asks"][0]

    async def test_repeats_on_each_poll(self, env):
        """Open asks reappear every poll until acked."""
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%50")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        for _ in range(3):
            r = await client.get("/asks/pending?pane_id=%2550")
            assert len(r.json()["asks"]) == 1

        # After ack, gone
        await client.post("/ack", json={"correlation_id": cid, "from_peer": bob})
        r = await client.get("/asks/pending?pane_id=%2550")
        assert r.json()["asks"] == []

    async def test_unknown_pane(self, env):
        client, _, _, _ = env
        r = await client.get("/asks/pending?pane_id=%25nope")
        assert r.status_code == 404


class TestDeprecatedNoOps:
    """Compat: legacy transports may still POST these. Should return 200 silently."""

    async def test_picked_up_returns_200(self, env):
        client, _, _, _ = env
        r = await client.post("/asks/legacy-cid/picked_up", json={"correlation_id": "legacy-cid"})
        assert r.status_code == 200

    async def test_mark_reminded_returns_200(self, env):
        client, _, _, _ = env
        r = await client.post(
            "/asks/legacy-cid/mark_reminded",
            json={"correlation_id": "legacy-cid"},
        )
        assert r.status_code == 200
