"""Tests for daemon HTTP routes (peers, messages, events)."""

from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import health, messages, peers
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_test_app(tmp_path: Path):
    """Build minimal app with deps initialized (no lifespan needed)."""
    cfg = Config()
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
    # Override events path to avoid loading real events
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()

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
    app.include_router(health.router)
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(spawn_routes.router)
    return app


@pytest.fixture
async def client(tmp_path):
    """Async HTTP test client with deps initialized."""
    app = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c
    cleanup_deps()


# -- Health --


class TestHealth:
    async def test_health(self, client):
        r = await client.get("/health")
        assert r.status_code == 200
        assert r.json()["status"] == "ok"


# -- Peers --


class TestPeers:
    async def test_list_peers_empty(self, client):
        r = await client.get("/peers")
        assert r.status_code == 200
        assert r.json()["peers"] == []

    async def test_register_peer(self, client):
        r = await client.post("/peers", json={
            "name": "testpeer",
            "display_name": "testpeer",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        })
        assert r.status_code == 200

        r = await client.get("/peers")
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["display_name"] == "testpeer"

    async def test_get_peer_by_name(self, client):
        await client.post("/peers", json={
            "name": "mypeer",
            "display_name": "mypeer",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        })
        r = await client.get("/peers/mypeer")
        assert r.status_code == 200
        assert r.json()["display_name"] == "mypeer"

    async def test_get_peer_not_found(self, client):
        r = await client.get("/peers/nonexistent")
        assert r.status_code == 404

    async def test_delete_peer(self, client):
        await client.post("/peers", json={
            "name": "delpeer",
            "display_name": "delpeer",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        })
        r = await client.delete("/peers/delpeer")
        assert r.status_code == 200

        r = await client.get("/peers/delpeer")
        assert r.status_code == 404

    async def test_set_description(self, client):
        await client.post("/peers", json={
            "name": "descpeer",
            "display_name": "descpeer",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        })
        r = await client.post("/peers/descpeer/description", json={
            "description": "working on tests",
        })
        assert r.status_code == 200

        r = await client.get("/peers/descpeer")
        assert r.json()["description"] == "working on tests"

    async def test_register_duplicate_peer(self, client):
        payload = {
            "name": "dup",
            "display_name": "dup",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        }
        await client.post("/peers", json=payload)
        r = await client.post("/peers", json=payload)
        assert r.status_code == 200

        r = await client.get("/peers")
        names = [p["display_name"] for p in r.json()["peers"]]
        assert names.count("dup") == 1


# -- Events --


class TestEvents:
    async def test_get_events_empty(self, client):
        r = await client.get("/events")
        assert r.status_code == 200
        assert r.json() == []

    async def test_post_chat_turn(self, client):
        r = await client.post("/events/chat", json={
            "peer": "testpeer",
            "role": "user",
            "text": "hello",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        assert events[0]["type"] == "chat_turn"
        assert events[0]["peer"] == "testpeer"
        assert events[0]["text"] == "hello"

    async def test_chat_turn_with_tool_calls(self, client):
        r = await client.post("/events/chat", json={
            "peer": "testpeer",
            "role": "assistant",
            "text": "Done",
            "tool_calls": [
                {"name": "Bash", "input": "echo hello"},
                {"name": "Read", "input": "auth.py"},
            ],
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        assert events[0]["tool_calls"] == [
            {"name": "Bash", "input": "echo hello"},
            {"name": "Read", "input": "auth.py"},
        ]

    async def test_chat_turn_without_tool_calls(self, client):
        r = await client.post("/events/chat", json={
            "peer": "testpeer",
            "role": "assistant",
            "text": "No tools used",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert events[0].get("tool_calls") is None

    async def test_events_have_id_and_timestamp(self, client):
        await client.post("/events/chat", json={
            "peer": "p", "role": "user", "text": "hi",
        })
        r = await client.get("/events")
        event = r.json()[0]
        assert "id" in event
        assert "timestamp" in event


# -- Notify --


class TestNotify:
    async def test_notify_unknown_peer(self, client):
        r = await client.post("/notify", json={
            "from_peer": "sender",
            "to_peer": "nonexistent",
            "text": "hello",
        })
        assert r.status_code == 404


# -- Broadcast --


class TestBroadcast:
    async def test_broadcast_no_peers(self, client):
        r = await client.post("/broadcast", json={
            "from_peer": "sender",
            "text": "hello all",
        })
        assert r.status_code == 200
        assert r.json()["sent_to"] == []


# -- Session Update --


class TestSessionUpdate:
    async def test_update_by_peer_name(self, client):
        await client.post("/peers", json={
            "name": "statuspeer",
            "display_name": "statuspeer",
            "path": "/tmp/test",
            "circle": "default",
            "backend": "claude-code",
        })
        r = await client.post("/session/update", json={
            "peer_name": "statuspeer",
            "status": "busy",
        })
        assert r.status_code == 200

        r = await client.get("/peers/statuspeer")
        assert r.json()["status"] == "busy"
