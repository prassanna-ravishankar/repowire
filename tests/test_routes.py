"""Tests for daemon HTTP routes (peers, messages, events)."""

import time
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, get_peer_registry, init_deps
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
    # Disable lazy_repair's demote logic (no WS in tests would mark all peers offline)
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
            "path": "/tmp/testpeer",
            "circle": "default",
            "backend": "claude-code",
        })
        assert r.status_code == 200
        name = r.json()["display_name"]
        assert name == "testpeer-claude-code"

        r = await client.get("/peers")
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["display_name"] == name

    async def test_register_peer_with_pane_id(self, client):
        r = await client.post("/peers", json={
            "name": "panepeer",
            "path": "/tmp/panepeer",
            "circle": "default",
            "backend": "claude-code",
            "pane_id": "%77",
        })
        assert r.status_code == 200

        r = await client.get("/peers/by-pane/%2577")
        assert r.status_code == 200
        assert r.json()["display_name"] == "panepeer-claude-code"

    async def test_get_peer_by_name(self, client):
        r = await client.post("/peers", json={
            "name": "mypeer",
            "path": "/tmp/mypeer",
            "circle": "default",
            "backend": "claude-code",
        })
        name = r.json()["display_name"]
        r = await client.get(f"/peers/{name}")
        assert r.status_code == 200
        assert r.json()["display_name"] == name

    async def test_get_peer_not_found(self, client):
        r = await client.get("/peers/nonexistent")
        assert r.status_code == 404

    async def test_delete_peer(self, client):
        r = await client.post("/peers", json={
            "name": "delpeer",
            "path": "/tmp/delpeer",
            "circle": "default",
            "backend": "claude-code",
        })
        name = r.json()["display_name"]
        r = await client.delete(f"/peers/{name}")
        assert r.status_code == 200

        r = await client.get(f"/peers/{name}")
        assert r.status_code == 404

    async def test_set_description(self, client):
        r = await client.post("/peers", json={
            "name": "descpeer",
            "path": "/tmp/descpeer",
            "circle": "default",
            "backend": "claude-code",
        })
        name = r.json()["display_name"]
        r = await client.post(f"/peers/{name}/description", json={
            "description": "working on tests",
        })
        assert r.status_code == 200

        r = await client.get(f"/peers/{name}")
        assert r.json()["description"] == "working on tests"

    async def test_register_duplicate_peer(self, client):
        payload = {
            "name": "dup",
            "path": "/tmp/dup",
            "circle": "default",
            "backend": "claude-code",
        }
        r1 = await client.post("/peers", json=payload)
        name = r1.json()["display_name"]
        r = await client.post("/peers", json=payload)
        assert r.status_code == 200

        r = await client.get("/peers")
        names = [p["display_name"] for p in r.json()["peers"]]
        assert names.count(name) == 1

    async def test_list_peers_status_filter(self, client):
        r = await client.post("/peers", json={
            "name": "onlinepeer",
            "path": "/tmp/onlinepeer",
            "circle": "default",
            "backend": "claude-code",
        })
        online_name = r.json()["display_name"]

        r = await client.post("/peers", json={
            "name": "offlinepeer",
            "path": "/tmp/offlinepeer",
            "circle": "default",
            "backend": "claude-code",
        })
        offline_name = r.json()["display_name"]

        r = await client.post("/session/update", json={
            "peer_name": offline_name,
            "status": "offline",
        })
        assert r.status_code == 200

        r = await client.get("/peers")
        assert len(r.json()["peers"]) == 2

        r = await client.get("/peers", params={"status": "online"})
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["display_name"] == online_name

        r = await client.get("/peers", params={"status": "offline"})
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["display_name"] == offline_name

    async def test_list_peers_path_filter_follows_symlinks(self, client, tmp_path):
        real = tmp_path / "real"
        real.mkdir()
        link = tmp_path / "link"
        link.symlink_to(real)

        await client.post("/peers", json={
            "name": "viasymlink", "path": str(link),
            "circle": "default", "backend": "claude-code",
        })

        r = await client.get("/peers", params={"path": str(real)})
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["backend"] == "claude-code"

    async def test_list_peers_path_backend_filter(self, client):
        await client.post("/peers", json={
            "name": "alpha", "path": "/tmp/proj-x",
            "circle": "default", "backend": "claude-code",
        })
        await client.post("/peers", json={
            "name": "beta", "path": "/tmp/proj-x",
            "circle": "default", "backend": "codex",
        })
        await client.post("/peers", json={
            "name": "gamma", "path": "/tmp/proj-y",
            "circle": "default", "backend": "claude-code",
        })

        r = await client.get("/peers", params={"path": "/tmp/proj-x"})
        assert {p["backend"] for p in r.json()["peers"]} == {"claude-code", "codex"}
        assert all(p["path"] == "/tmp/proj-x" for p in r.json()["peers"])

        r = await client.get("/peers", params={"backend": "codex"})
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["backend"] == "codex"

        r = await client.get(
            "/peers", params={"path": "/tmp/proj-x", "backend": "claude-code"},
        )
        peers = r.json()["peers"]
        assert len(peers) == 1
        assert peers[0]["path"] == "/tmp/proj-x"
        assert peers[0]["backend"] == "claude-code"


# -- Spawn / kill --


class TestKillPeer:
    async def test_kill_peer_by_peer_id_resolves_tmux_session(self, client, monkeypatch):
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
        )
        killed: list[str] = []

        def fake_kill(tmux_session: str) -> bool:
            killed.append(tmux_session)
            return True

        monkeypatch.setattr(spawn_routes, "kill_peer", fake_kill)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json()["ok"] is True
        assert killed == ["5:torale-window"]

    async def test_kill_peer_by_name_and_circle(self, client, monkeypatch):
        registry = get_peer_registry()
        _peer_id, display_name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
        )
        killed: list[str] = []

        def fake_kill(tmux_session: str) -> bool:
            killed.append(tmux_session)
            return True

        monkeypatch.setattr(spawn_routes, "kill_peer", fake_kill)

        r = await client.post(
            "/kill-peer",
            json={"peer_identifier": display_name, "circle": "5"},
        )

        assert r.status_code == 200
        assert killed == ["5:torale-window"]

    async def test_kill_peer_ambiguous_display_name_returns_409(self, client, monkeypatch):
        registry = get_peer_registry()
        await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-a",
        )
        await registry.allocate_and_register(
            circle="6",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="6:torale-b",
        )

        def fail_kill(tmux_session: str) -> bool:
            raise AssertionError(f"should not kill ambiguous peer: {tmux_session}")

        monkeypatch.setattr(spawn_routes, "kill_peer", fail_kill)

        r = await client.post(
            "/kill-peer",
            json={"peer_identifier": "torale-claude-code"},
        )

        assert r.status_code == 409
        detail = r.json()["detail"]
        assert "Ambiguous peer identifier" in detail["error"]
        assert {p["circle"] for p in detail["candidates"]} == {"5", "6"}

    async def test_kill_peer_without_tmux_session_unregisters(self, client):
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
        )

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_with_dead_tmux_session_unregisters(self, client, monkeypatch):
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-stale",
        )
        monkeypatch.setattr(spawn_routes, "kill_peer", lambda _: False)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_prefers_pane_id_when_available(self, client, monkeypatch):
        """Spawned peers (tmux_session + pane_id both set) kill via pane_id."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%42",
        )
        pane_killed: list[str] = []
        window_killed: list[str] = []

        monkeypatch.setattr(
            spawn_routes,
            "kill_pane",
            lambda pid: pane_killed.append(pid) or True,
        )
        monkeypatch.setattr(
            spawn_routes,
            "kill_peer",
            lambda ts: window_killed.append(ts) or True,
        )

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["tmux_killed"] is True
        assert pane_killed == ["%42"]
        assert window_killed == []

    async def test_kill_peer_externally_attached_skips_tmux_kill(self, client, monkeypatch):
        """Peer with pane_id but no tmux_session (SessionStart-registered)
        is externally attached — daemon must not touch its tmux pane."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            pane_id="%99",
            # NOTE: tmux_session intentionally omitted — mirrors SessionStart hook
        )

        def must_not_call(*_args):
            raise AssertionError("must not touch tmux for externally-attached peer")

        monkeypatch.setattr(spawn_routes, "kill_pane", must_not_call)
        monkeypatch.setattr(spawn_routes, "kill_peer", must_not_call)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["tmux_killed"] is None
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_orphan_pane_surfaces_false(self, client, monkeypatch):
        """If kill_pane fails (e.g. pane already gone), tmux_killed=False but
        peer is still unregistered."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%7",
        )
        monkeypatch.setattr(spawn_routes, "kill_pane", lambda _: False)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        body = r.json()
        assert body["tmux_killed"] is False
        assert await registry.get_peer(peer_id) is None


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

    async def test_chat_turn_with_explicit_peer_id(self, client):
        """Chat turn with peer_id passed directly should store it in the event."""
        r = await client.post("/events/chat", json={
            "peer": "testpeer",
            "role": "user",
            "text": "hello",
            "peer_id": "repow-default-abc12345",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        assert events[0]["peer_id"] == "repow-default-abc12345"

    async def test_chat_turn_resolves_peer_id_from_pane_id(self, client):
        """Chat turn with pane_id should resolve peer_id from registry."""
        from repowire.config.models import AgentType
        from repowire.daemon.deps import get_peer_registry
        registry = get_peer_registry()
        _peer_id, _name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/panepeer",
            pane_id="%99",
        )

        r = await client.post("/events/chat", json={
            "peer": "panepeer",
            "role": "assistant",
            "text": "done",
            "pane_id": "%99",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        assert events[0]["peer_id"] is not None
        assert events[0]["peer_id"].startswith("repow-")

    async def test_chat_turn_without_peer_id_or_pane_id(self, client):
        """Chat turn without peer_id or pane_id should still work (legacy compat)."""
        r = await client.post("/events/chat", json={
            "peer": "legacypeer",
            "role": "user",
            "text": "old style",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        assert events[0].get("peer_id") is None


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
        r = await client.post("/peers", json={
            "name": "statuspeer",
            "path": "/tmp/statuspeer",
            "circle": "default",
            "backend": "claude-code",
        })
        name = r.json()["display_name"]
        r = await client.post("/session/update", json={
            "peer_name": name,
            "status": "busy",
        })
        assert r.status_code == 200

        r = await client.get(f"/peers/{name}")
        assert r.json()["status"] == "busy"
