"""Tests for daemon HTTP routes (peers, messages, events)."""

from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, get_peer_registry
from repowire.daemon.routes import health, messages, peers
from repowire.daemon.routes import spawn as spawn_routes
from repowire.protocol.peers import PeerStatus
from repowire.spawn_ownership import TmuxPaneEvidence

from .conftest import async_client_for, make_daemon_app

ROUTERS = (health.router, peers.router, messages.router, spawn_routes.router)


@pytest.fixture
async def client(tmp_path):
    """Async HTTP test client with deps initialized."""
    harness = make_daemon_app(tmp_path, ROUTERS)
    async with async_client_for(harness.app) as c:
        yield c
    cleanup_deps()


# -- Health --


class TestHealth:
    async def test_health(self, client):
        r = await client.get("/health")
        assert r.status_code == 200
        body = r.json()
        assert body["status"] == "ok"
        assert "channel" in body
        assert "acp_broker" in body
        assert body["acp_broker"]["status"] == "inactive"

    async def test_health_relay_disabled_when_no_client(self, client):
        # No relay client on app.state and relay_mode false => disabled.
        r = await client.get("/health")
        body = r.json()
        assert body["relay"]["status"] == "disabled"
        assert body["relay"]["connected"] is False

    async def test_health_relay_down_when_enabled_but_client_dead(self):
        # The bug this guards: relay enabled but the client is not connected.
        # Old /health reported relay_mode:true and hid it; now relay.status=down.
        class DeadRelay:
            def __init__(self):
                self.ensured = False

            async def ensure_running(self):
                self.ensured = True
                return False

            def status(self):
                return {
                    "connected": False,
                    "running": False,
                    "url": "wss://relay.example",
                    "last_connected_at": "2026-06-20T17:44:44+00:00",
                    "last_error": "ConnectionClosedError: no close frame",
                    "last_error_at": "2026-06-20T17:44:40+00:00",
                }

        dead = DeadRelay()
        app = FastAPI()
        app.state.config = Config()
        app.state.relay_mode = True
        app.state.relay_client = dead
        app.include_router(health.router)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            r = await c.get("/health")
        body = r.json()
        assert body["relay_mode"] is True  # config flag still true...
        assert body["relay"]["status"] == "down"  # ...but truth is exposed
        assert body["relay"]["enabled"] is True
        assert body["relay"]["connected"] is False
        assert body["relay"]["last_error"].startswith("ConnectionClosedError")
        assert dead.ensured is True  # lazy self-heal was attempted

    async def test_health_relay_connected(self):
        class LiveRelay:
            async def ensure_running(self):
                return False

            def status(self):
                return {
                    "connected": True,
                    "running": True,
                    "url": "wss://relay.example",
                    "last_connected_at": "2026-06-21T03:00:00+00:00",
                    "last_error": None,
                    "last_error_at": None,
                }

        app = FastAPI()
        app.state.config = Config()
        app.state.relay_mode = True
        app.state.relay_client = LiveRelay()
        app.include_router(health.router)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            r = await c.get("/health")
        body = r.json()
        assert body["relay"]["status"] == "connected"
        assert body["relay"]["connected"] is True

    async def test_health_reports_acp_broker_snapshot(self, monkeypatch):
        monkeypatch.setattr("repowire.daemon.routes.health.shutil.which", lambda _tool: None)
        monkeypatch.setattr(
            "repowire.daemon.routes.health.importlib.util.find_spec",
            lambda _name: object(),
        )
        cfg = Config()
        cfg.experiments.acp_broker_client = True

        class FakeManager:
            def health_snapshot(self):
                return {
                    "manager_initialized": True,
                    "active_clients": 1,
                    "in_flight": 2,
                    "last_error": None,
                }

        class FakePermissionBroker:
            def health_snapshot(self):
                return {"pending": 1, "timeout_seconds": 60.0, "last_error": None}

        class FakeRegistry:
            async def get_all_peers(self):
                return [
                    SimpleNamespace(metadata={"acp": {"command": "codex-acp"}}),
                    SimpleNamespace(metadata={}),
                ]

        app = FastAPI()
        app.state.config = cfg
        app.state.peer_registry = FakeRegistry()
        app.state.acp_manager = FakeManager()
        app.state.acp_permission_broker = FakePermissionBroker()
        app.include_router(health.router)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            r = await c.get("/health")
        assert r.status_code == 200
        body = r.json()
        assert body["acp_broker"]["status"] == "busy"
        assert body["acp_broker"]["configured_peers"] == 1
        assert body["acp_broker"]["active_clients"] == 1
        assert body["acp_broker"]["in_flight"] == 2
        assert body["acp_broker"]["permissions"]["pending"] == 1


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

    async def test_get_peer_ambiguous_display_name_returns_409(self, client):
        for circle in ("team-a", "team-b"):
            r = await client.post("/peers", json={
                "name": "shared",
                "path": "/tmp/shared",
                "circle": circle,
                "backend": "claude-code",
            })
            assert r.status_code == 200
            assert r.json()["display_name"] == "shared-claude-code"

        r = await client.get("/peers/shared-claude-code")
        assert r.status_code == 409
        detail = r.json()["detail"]
        assert "Ambiguous peer name" in detail
        assert "Specify a circle=" in detail
        assert "peer_id" in detail

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

    async def test_register_pane_hijack_returns_409(self, client):
        """A subprocess agent (parent_pid matches the live pane peer's
        agent_pid) gets 409 instead of stealing the pane. See issue #190."""
        r = await client.post("/peers", json={
            "name": "parent",
            "path": "/tmp/parent-proj",
            "circle": "default",
            "backend": "claude-code",
            "pane_id": "%hijack",
            "agent_pid": 41001,
            "parent_pid": 40000,
        })
        assert r.status_code == 200
        parent_name = r.json()["display_name"]

        r = await client.post("/peers", json={
            "name": "child",
            "path": "/tmp/child-cwd",
            "circle": "default",
            "backend": "gemini",
            "pane_id": "%hijack",
            "agent_pid": 41002,
            "parent_pid": 41001,
        })
        assert r.status_code == 409
        assert "%hijack" in r.json()["detail"]

        r = await client.get("/peers/by-pane/%25hijack")
        assert r.status_code == 200
        assert r.json()["display_name"] == parent_name

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

    async def test_list_peers_circle_filter(self, client):
        r = await client.post("/peers", json={
            "name": "alpha", "path": "/tmp/alpha",
            "circle": "team-a", "backend": "claude-code",
        })
        alpha = r.json()["display_name"]
        r = await client.post("/peers", json={
            "name": "beta", "path": "/tmp/beta",
            "circle": "team-b", "backend": "claude-code",
        })
        beta = r.json()["display_name"]
        # Service peer bypasses circles — should appear under any circle filter.
        r = await client.post("/peers", json={
            "name": "telegram", "path": "/tmp/telegram",
            "circle": "global", "backend": "claude-code",
            "role": "service",
        })
        telegram = r.json()["display_name"]

        # No circle param: back-compat — return everyone.
        r = await client.get("/peers")
        assert {p["display_name"] for p in r.json()["peers"]} == {alpha, beta, telegram}

        # Explicit '*' is mesh-wide.
        r = await client.get("/peers", params={"circle": "*"})
        assert {p["display_name"] for p in r.json()["peers"]} == {alpha, beta, telegram}

        # Concrete circle: returns that circle plus bypass-roles.
        r = await client.get("/peers", params={"circle": "team-a"})
        assert {p["display_name"] for p in r.json()["peers"]} == {alpha, telegram}
        r = await client.get("/peers", params={"circle": "team-b"})
        assert {p["display_name"] for p in r.json()["peers"]} == {beta, telegram}

        # A circle no peer belongs to still surfaces bypass-roles.
        r = await client.get("/peers", params={"circle": "nobody"})
        assert {p["display_name"] for p in r.json()["peers"]} == {telegram}


# -- Spawn / kill --


class TestKillPeer:
    """kill_peer route tests.

    Ownership model: a peer's pane is killed iff Repowire has spawn ownership
    proof or live pane metadata whose peer_id matches the target peer.
    """

    @pytest.fixture(autouse=True)
    def _reset_spawned_panes(self):
        spawn_routes._SPAWNED_PANE_IDS.clear()
        yield
        spawn_routes._SPAWNED_PANE_IDS.clear()

    async def test_kill_peer_by_peer_id_kills_spawned_pane(self, client, monkeypatch):
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%42",
        )
        spawn_routes._SPAWNED_PANE_IDS.add("%42")
        killed: list[str] = []
        monkeypatch.setattr(
            spawn_routes, "kill_pane", lambda pid: killed.append(pid) or True,
        )

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json() == {"ok": True, "tmux_killed": True}
        assert killed == ["%42"]
        assert "%42" not in spawn_routes._SPAWNED_PANE_IDS  # cleared after kill

    async def test_kill_peer_by_name_and_circle(self, client, monkeypatch):
        registry = get_peer_registry()
        _peer_id, display_name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%42",
        )
        spawn_routes._SPAWNED_PANE_IDS.add("%42")
        killed: list[str] = []
        monkeypatch.setattr(
            spawn_routes, "kill_pane", lambda pid: killed.append(pid) or True,
        )

        r = await client.post(
            "/kill-peer",
            json={"peer_identifier": display_name, "circle": "5"},
        )

        assert r.status_code == 200
        assert killed == ["%42"]

    async def test_kill_peer_ambiguous_display_name_returns_409(self, client, monkeypatch):
        registry = get_peer_registry()
        await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-a",
            pane_id="%1",
        )
        await registry.allocate_and_register(
            circle="6",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="6:torale-b",
            pane_id="%2",
        )

        def fail_kill(*_args):
            raise AssertionError("should not kill ambiguous peer")

        monkeypatch.setattr(spawn_routes, "kill_pane", fail_kill)

        r = await client.post(
            "/kill-peer",
            json={"peer_identifier": "torale-claude-code"},
        )

        assert r.status_code == 409
        detail = r.json()["detail"]
        assert "Ambiguous peer identifier" in detail["error"]
        assert {p["circle"] for p in detail["candidates"]} == {"5", "6"}

    async def test_kill_peer_without_pane_id_deregisters_without_tmux(self, client):
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
        )

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json()["tmux_killed"] is None
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_opencode_attached_without_metadata_deregisters_only(
        self, client, monkeypatch
    ):
        """OpenCode plugin sends BOTH tmux_session and pane_id from any user
        tmux pane (installers/opencode.py:613-622, 213-216). Without a
        spawned-pane record, /kill-peer must NOT touch the user's tmux.
        """
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.OPENCODE,
            path="/tmp/torale",
            tmux_session="user-session:my-window",
            pane_id="%77",
        )
        # _SPAWNED_PANE_IDS deliberately empty — peer was attached, not spawned.
        # Live pane existence alone is still not enough to kill.
        monkeypatch.setattr(
            spawn_routes,
            "probe_tmux_pane",
            lambda pane_id: TmuxPaneEvidence(
                pane_id=pane_id,
                tmux_session="user-session:my-window",
                current_path="/tmp/torale",
                pane_pid="12345",
            ),
        )

        def must_not_call(*_args):
            raise AssertionError("must not touch tmux for externally-attached peer")

        monkeypatch.setattr(spawn_routes, "kill_pane", must_not_call)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json()["tmux_killed"] is None
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_claude_sessionstart_metadata_kills_manual_pane(
        self, client, monkeypatch
    ):
        """Matching live pane metadata is enough proof for manual tmux peers."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            pane_id="%99",
        )
        monkeypatch.setattr(
            spawn_routes,
            "probe_tmux_pane",
            lambda pane_id: TmuxPaneEvidence(
                pane_id=pane_id,
                tmux_session="5:torale-window",
                current_path="/tmp/torale",
                pane_pid="12345",
            ),
        )
        monkeypatch.setattr(
            spawn_routes,
            "read_pane_runtime_metadata",
            lambda _pane_id: {"peer_id": peer_id},
        )
        killed: list[str] = []
        monkeypatch.setattr(spawn_routes, "kill_pane", lambda pid: killed.append(pid) or True)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json()["tmux_killed"] is True
        assert killed == ["%99"]
        assert await registry.get_peer(peer_id) is None

    async def test_kill_peer_verified_pane_kill_failure_is_loud(self, client, monkeypatch):
        """If kill_pane fails after verification, leave the peer registered."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%7",
        )
        spawn_routes._SPAWNED_PANE_IDS.add("%7")
        monkeypatch.setattr(spawn_routes, "kill_pane", lambda _: False)

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 500
        assert r.json()["detail"]["error"] == "kill_failed"
        assert await registry.get_peer(peer_id) is not None
        assert "%7" in spawn_routes._SPAWNED_PANE_IDS

    async def test_kill_peer_post_restart_without_metadata_deregisters_only(
        self, client, monkeypatch,
    ):
        """After daemon restart, pane id alone is not destructive proof."""
        registry = get_peer_registry()
        peer_id, _name = await registry.allocate_and_register(
            circle="5",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/torale",
            tmux_session="5:torale-window",
            pane_id="%55",
        )
        # _SPAWNED_PANE_IDS empty (simulates post-restart state)

        monkeypatch.setattr(
            spawn_routes,
            "kill_pane",
            lambda _: (_ for _ in ()).throw(AssertionError("must not call")),
        )

        r = await client.post("/kill-peer", json={"peer_identifier": peer_id})

        assert r.status_code == 200
        assert r.json()["tmux_killed"] is None
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

    async def test_post_chat_turn_delta(self, client):
        """A chat_turn_delta should land as a chat_turn_delta event with all fields."""
        r = await client.post("/events/chat_delta", json={
            "peer": "streampeer",
            "role": "assistant",
            "session_id": "stream-session",
            "turn_id": "turn-abc",
            "chunk_index": 0,
            "kind": "text",
            "text": "Hello, I'll start by",
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert len(events) == 1
        e = events[0]
        assert e["type"] == "chat_turn_delta"
        assert e["session_id"] == "stream-session"
        assert e["turn_id"] == "turn-abc"
        assert e["chunk_index"] == 0
        assert e["kind"] == "text"
        assert e["text"] == "Hello, I'll start by"
        assert e["is_final"] is False

    async def test_chat_turn_delta_tool_use(self, client):
        r = await client.post("/events/chat_delta", json={
            "peer": "streampeer",
            "role": "assistant",
            "turn_id": "turn-xyz",
            "chunk_index": 1,
            "kind": "tool_use",
            "text": "Bash: ls -la",
            "tool_call": {"name": "Bash", "input": "ls -la"},
        })
        assert r.status_code == 200

        r = await client.get("/events")
        events = r.json()
        assert events[0]["kind"] == "tool_use"
        assert events[0]["tool_call"] == {"name": "Bash", "input": "ls -la"}

    async def test_chat_turn_delta_resolves_peer_id_from_pane_id(self, client):
        from repowire.config.models import AgentType
        from repowire.daemon.deps import get_peer_registry
        registry = get_peer_registry()
        await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/streampane",
            pane_id="%77",
        )

        r = await client.post("/events/chat_delta", json={
            "peer": "streampane",
            "role": "assistant",
            "turn_id": "turn-1",
            "chunk_index": 0,
            "text": "partial",
            "pane_id": "%77",
        })
        assert r.status_code == 200

        events = (await client.get("/events")).json()
        assert events[0]["peer_id"] is not None
        assert events[0]["peer_id"].startswith("repow-")

    async def test_chat_turn_delta_rejects_negative_chunk_index(self, client):
        r = await client.post("/events/chat_delta", json={
            "peer": "p",
            "turn_id": "t",
            "chunk_index": -1,
            "text": "x",
        })
        assert r.status_code == 422

    async def test_chat_turn_carries_turn_id(self, client):
        r = await client.post("/events/chat", json={
            "peer": "testpeer",
            "role": "assistant",
            "text": "final",
            "session_id": "session-9",
            "turn_id": "msg-uuid-9",
        })
        assert r.status_code == 200
        events = (await client.get("/events")).json()
        assert events[0]["session_id"] == "session-9"
        assert events[0]["turn_id"] == "msg-uuid-9"

    async def test_tool_use_turn_canonical_id_drops_deltas(self, client):
        """Multi-assistant tool turn: streamer emits deltas under the *first*
        assistant uuid (a1); Stop posts final with the same canonical turn_id.
        Late deltas (e.g. arriving after Stop) must still be dropped."""
        # Deltas under a1 (streamer's view).
        r = await client.post("/events/chat_delta", json={
            "peer": "toolpeer",
            "role": "assistant",
            "turn_id": "a1",
            "chunk_index": 0,
            "kind": "text",
            "text": "I'll check",
        })
        assert r.status_code == 200
        r = await client.post("/events/chat_delta", json={
            "peer": "toolpeer",
            "role": "assistant",
            "turn_id": "a1",
            "chunk_index": 1,
            "kind": "tool_use",
            "text": "Bash: ls",
            "tool_call": {"name": "Bash", "input": "ls"},
        })
        assert r.status_code == 200
        # Stop fires; transcript now has a1 (tool_use) then a2 (final text).
        # Canonical turn_id is a1 — Stop must post a1, not a2.
        r = await client.post("/events/chat", json={
            "peer": "toolpeer",
            "role": "assistant",
            "text": "I'll check\nls -la output",
            "turn_id": "a1",
        })
        assert r.status_code == 200
        # Late delta after Stop — also under a1.
        r = await client.post("/events/chat_delta", json={
            "peer": "toolpeer",
            "role": "assistant",
            "turn_id": "a1",
            "chunk_index": 2,
            "kind": "text",
            "text": "(racing)",
        })
        assert r.status_code == 200
        events = (await client.get("/events")).json()
        finals = [e for e in events if e["type"] == "chat_turn"]
        deltas = [e for e in events if e["type"] == "chat_turn_delta"]
        late_deltas = [d for d in deltas if d["chunk_index"] == 2]
        assert len(finals) == 1
        assert finals[0]["turn_id"] == "a1"
        assert not late_deltas, "post-final delta with canonical turn_id must be dropped"

    async def test_late_delta_dropped_after_final(self, client):
        """A delta posted for a turn_id that already has a final chat_turn
        must not land as a chat_turn_delta event."""
        # Final arrives first (covers the race where Stop wins).
        r = await client.post("/events/chat", json={
            "peer": "racepeer",
            "role": "assistant",
            "text": "complete",
            "turn_id": "race-turn-1",
        })
        assert r.status_code == 200
        # Late delta for the same turn_id.
        r = await client.post("/events/chat_delta", json={
            "peer": "racepeer",
            "role": "assistant",
            "turn_id": "race-turn-1",
            "chunk_index": 3,
            "text": "stale block",
        })
        assert r.status_code == 200
        events = (await client.get("/events")).json()
        deltas = [e for e in events if e["type"] == "chat_turn_delta"]
        finals = [e for e in events if e["type"] == "chat_turn"]
        assert len(finals) == 1
        assert deltas == [], "late delta for finalized turn must be dropped"

    async def test_delta_before_final_is_kept(self, client):
        """In-order delta then final: both events land, dashboard reconciles."""
        r = await client.post("/events/chat_delta", json={
            "peer": "p",
            "role": "assistant",
            "turn_id": "ordered-turn-1",
            "chunk_index": 0,
            "text": "streaming",
        })
        assert r.status_code == 200
        r = await client.post("/events/chat", json={
            "peer": "p",
            "role": "assistant",
            "text": "final",
            "turn_id": "ordered-turn-1",
        })
        assert r.status_code == 200
        events = (await client.get("/events")).json()
        assert any(e["type"] == "chat_turn_delta" for e in events)
        assert any(e["type"] == "chat_turn" for e in events)

    async def test_finalized_set_capacity_bounded(self, client):
        """The finalized turn_id set must not grow without bound."""
        from repowire.daemon.routes import messages as msgs_mod

        msgs_mod._finalized_turn_ids.clear()
        msgs_mod._FINALIZED_TURN_IDS_CAPACITY = 5
        try:
            for i in range(20):
                r = await client.post("/events/chat", json={
                    "peer": "p",
                    "role": "assistant",
                    "text": "x",
                    "turn_id": f"cap-{i}",
                })
                assert r.status_code == 200
            assert len(msgs_mod._finalized_turn_ids) == 5
            # Oldest evicted.
            assert "legacy:cap-0" not in msgs_mod._finalized_turn_ids
            assert "legacy:cap-19" in msgs_mod._finalized_turn_ids
        finally:
            msgs_mod._finalized_turn_ids.clear()
            msgs_mod._FINALIZED_TURN_IDS_CAPACITY = 4096


# -- Notify --


class TestNotify:
    async def test_notify_unknown_peer(self, client):
        r = await client.post("/notify", json={
            "from_peer": "sender",
            "to_peer": "nonexistent",
            "text": "hello",
        })
        assert r.status_code == 404
        body = r.json()
        assert body["detail"] == "Unknown peer: nonexistent"
        assert body["ok"] is False
        assert body["status"] == "not_found"
        assert body["delivery_state"] == "unknown_peer"
        assert body["reason"] == "unknown_peer"
        assert body["delivered"] is False
        assert body["queued"] is False

    async def test_notify_ambiguous_peer_returns_409(self, client):
        """When two peers share a display_name across circles, /notify must
        return 409 (matches peer/description/touch/ask) instead of 404 or 500.
        See codex round-1 review on PR #195.
        """
        registry = get_peer_registry()
        await registry.allocate_and_register(
            circle="alpha",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/twin",
        )
        await registry.allocate_and_register(
            circle="beta",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/twin",
        )

        r = await client.post("/notify", json={
            "from_peer": "sender",
            "to_peer": "twin-claude-code",
            "text": "hello",
        })
        assert r.status_code == 409
        body = r.json()
        assert "Ambiguous peer name" in body["detail"]
        assert body["ok"] is False
        assert body["status"] == "ambiguous_peer"
        assert body["delivery_state"] == "failed"
        assert body["reason"] == "ambiguous_peer"
        assert body["delivered"] is False
        assert body["queued"] is False

    async def test_notify_cross_circle_forbidden_returns_403_shape(self, client):
        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="alpha",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/alpha-sender",
        )
        _recipient_id, recipient_name = await registry.allocate_and_register(
            circle="beta",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/beta-recipient",
        )

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "hello",
        })

        assert r.status_code == 403
        body = r.json()
        assert "Circle boundary" in body["detail"]
        assert body["ok"] is False
        assert body["status"] == "forbidden"
        assert body["delivery_state"] == "failed"
        assert body["reason"] == "forbidden"
        assert body["delivered"] is False
        assert body["queued"] is False

    async def test_notify_online_recipient_returns_sent(self, client, monkeypatch):
        from unittest.mock import AsyncMock

        from repowire.protocol.peers import PeerStatus

        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/sender",
        )
        recipient_id, recipient_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/recipient",
        )
        # Recipient is ONLINE (default from allocate_and_register)
        registry._peers[recipient_id].status = PeerStatus.ONLINE
        monkeypatch.setattr(
            registry._router, "send_notification", AsyncMock(return_value=None),
        )

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "hello",
        })
        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["status"] == "sent"
        assert body["delivery_state"] == "delivered"
        assert body["delivered"] is True
        assert body["queued"] is False
        assert body["reason"] == "transport_delivered"
        assert body["from_peer_id"] == _sender_id
        assert body["from_peer_name"] == sender_name
        assert body["to_peer_id"] == recipient_id
        assert body["to_peer_name"] == recipient_name

    async def test_notify_preserves_legacy_response_with_hook_delivery_ack(
        self, client, monkeypatch,
    ):
        from unittest.mock import AsyncMock

        from repowire.protocol.peers import PeerStatus

        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/sender-hook-delivery",
        )
        _recipient_id, recipient_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/recipient-hook-delivery",
        )
        registry._peers[_recipient_id].status = PeerStatus.ONLINE
        hook_ack = {
            "type": "delivery_ack",
            "delivery_id": "notif-delivery-abc",
            "message_type": "notify",
            "status": "injected",
        }
        monkeypatch.setattr(
            registry._router, "send_notification", AsyncMock(return_value=hook_ack),
        )

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "hello",
        })

        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["status"] == "sent"
        assert body["delivery_state"] == "delivered"
        assert body["delivered"] is True
        assert body["reason"] == "transport_delivered"
        assert body["hook_delivery"] == hook_ack

    async def test_notify_carries_attachments_to_event_and_wire(self, client, monkeypatch):
        from unittest.mock import AsyncMock

        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/sender-att",
        )
        _recipient_id, recipient_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/recipient-att",
        )
        monkeypatch.setattr(
            registry._router, "send_notification", AsyncMock(return_value=None),
        )

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "see file",
            "attachments": [{
                "id": "att123",
                "path": "/tmp/att123.png",
                "filename": "diagram.png",
            }],
        })

        assert r.status_code == 200
        kwargs = registry._router.send_notification.await_args.kwargs
        assert kwargs["attachments"][0].id == "att123"
        event = registry.get_events()[-1]
        assert event["attachments"][0]["filename"] == "diagram.png"

    async def test_notify_busy_recipient_is_delivered_immediately(self, client, monkeypatch):
        """A BUSY recipient still gets the message on the wire right away —
        its runtime's composer queues it until the turn ends. Queueing is the
        runtime's job; the daemon must not claim it held the message."""
        from unittest.mock import AsyncMock

        from repowire.protocol.peers import PeerStatus

        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/sender2",
        )
        recipient_id, recipient_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/recipient2",
        )
        registry._peers[recipient_id].status = PeerStatus.BUSY
        send_mock = AsyncMock(return_value=None)
        monkeypatch.setattr(registry._router, "send_notification", send_mock)

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "hello",
        })
        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["status"] == "sent"
        assert body["delivery_state"] == "delivered"
        assert body["delivered"] is True
        assert body["queued"] is False
        assert body["reason"] == "transport_delivered"
        assert body["from_peer_id"] == _sender_id
        assert body["from_peer_name"] == sender_name
        assert body["to_peer_id"] == recipient_id
        assert body["to_peer_name"] == recipient_name
        send_mock.assert_awaited_once()

    async def test_notify_no_live_transport_returns_503_shape(self, client, monkeypatch):
        from unittest.mock import AsyncMock

        from repowire.daemon.websocket_transport import TransportError

        registry = get_peer_registry()
        _sender_id, sender_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/sender3",
        )
        _recipient_id, recipient_name = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/recipient3",
        )
        monkeypatch.setattr(
            registry._router,
            "send_notification",
            AsyncMock(side_effect=TransportError("No connection")),
        )

        r = await client.post("/notify", json={
            "from_peer": sender_name,
            "to_peer": recipient_name,
            "text": "hello",
        })

        assert r.status_code == 503
        body = r.json()
        assert "no live connection" in body["detail"]
        assert body["ok"] is False
        assert body["status"] == "unavailable"
        assert body["delivery_state"] == "no_live_transport"
        assert body["reason"] == "no_live_transport"
        assert body["delivered"] is False
        assert body["queued"] is False
        peer = await registry.get_peer(recipient_name)
        assert peer is not None
        assert peer.status == PeerStatus.OFFLINE


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


@pytest.mark.asyncio
async def test_acp_notify_reason_is_broker_accepted():
    # codex ACP must-fix #3: a fire-and-forget notify routed over ACP must report
    # reason="broker_accepted" (the broker took the prompt task) rather than
    # "transport_delivered" (which means written to a live WS), so clients don't
    # mistake broker acceptance for a runtime receipt.
    from unittest.mock import AsyncMock

    from repowire.daemon.peer_delivery import PeerDeliveryService
    from repowire.daemon.transport_router import NotifyTransportResult

    sender = SimpleNamespace(peer_id="sender-id", display_name="sender-codex")
    target = SimpleNamespace(peer_id="target-id", display_name="target-codex")

    registry = SimpleNamespace(
        check_access=AsyncMock(return_value=(sender, target)),
        add_event=lambda *a, **k: None,
    )
    transport_router = SimpleNamespace(
        send_notify=AsyncMock(
            return_value=NotifyTransportResult(status="sent", transport="acp")
        )
    )
    service = PeerDeliveryService(
        registry=registry,  # type: ignore[arg-type]
        message_router=SimpleNamespace(),  # type: ignore[arg-type]
        transport_router=transport_router,  # type: ignore[arg-type]
    )

    result = await service.notify_result(
        from_peer="sender-codex", to_peer="target-codex", text="heads up",
    )
    assert result.delivery_state == "delivered"
    assert result.reason == "broker_accepted"
    assert result.transport == "acp"


@pytest.mark.asyncio
async def test_live_notify_gates_on_pending_first_turn():
    """A live notify to a WS peer still seeding (pending_first_turn) waits for
    the seed to settle before injecting, so it doesn't race the spawn seed."""
    from unittest.mock import AsyncMock, patch

    from repowire.daemon.peer_delivery import PeerDeliveryService
    from repowire.daemon.transport_router import NotifyTransportResult

    sender = SimpleNamespace(peer_id="sender-id", display_name="sender-cc")
    pending = SimpleNamespace(
        peer_id="target-id", display_name="target-cc", turn_state="pending_first_turn"
    )
    settled = SimpleNamespace(
        peer_id="target-id", display_name="target-cc", turn_state="idle"
    )

    snapshots = [pending, settled]
    calls = {"n": 0}

    async def get_peer(_session_id):
        idx = min(calls["n"], len(snapshots) - 1)
        calls["n"] += 1
        return snapshots[idx]

    registry = SimpleNamespace(
        check_access=AsyncMock(return_value=(sender, pending)),
        get_peer=get_peer,
        add_event=lambda *a, **k: None,
    )
    send_notify = AsyncMock(
        return_value=NotifyTransportResult(status="sent", transport="ws")
    )
    transport_router = SimpleNamespace(
        send_notify=send_notify,
        acp_route=lambda _target: None,  # WS route => gate applies
    )
    service = PeerDeliveryService(
        registry=registry,  # type: ignore[arg-type]
        message_router=SimpleNamespace(),  # type: ignore[arg-type]
        transport_router=transport_router,  # type: ignore[arg-type]
    )

    with patch("repowire.daemon.seed_gate.asyncio.sleep", new_callable=AsyncMock):
        result = await service.notify_result(
            from_peer="sender-cc", to_peer="target-cc", text="heads up",
        )

    # The gate re-polled the registry (pending -> settled) before sending.
    assert calls["n"] >= 2
    send_notify.assert_awaited_once()
    assert result.delivery_state == "delivered"


@pytest.mark.asyncio
async def test_live_notify_acp_target_skips_seed_gate():
    """ACP-routed targets don't inject into a pane, so the seed gate is a
    no-op for them even while pending_first_turn."""
    from unittest.mock import AsyncMock

    from repowire.daemon.peer_delivery import PeerDeliveryService
    from repowire.daemon.transport_router import NotifyTransportResult

    sender = SimpleNamespace(peer_id="sender-id", display_name="sender-codex")
    target = SimpleNamespace(
        peer_id="target-id", display_name="target-codex", turn_state="pending_first_turn"
    )

    get_peer = AsyncMock()  # must NOT be polled by the gate for an ACP target
    registry = SimpleNamespace(
        check_access=AsyncMock(return_value=(sender, target)),
        get_peer=get_peer,
        add_event=lambda *a, **k: None,
    )
    transport_router = SimpleNamespace(
        send_notify=AsyncMock(
            return_value=NotifyTransportResult(status="sent", transport="acp")
        ),
        acp_route=lambda _target: SimpleNamespace(),  # ACP route => gate skipped
    )
    service = PeerDeliveryService(
        registry=registry,  # type: ignore[arg-type]
        message_router=SimpleNamespace(),  # type: ignore[arg-type]
        transport_router=transport_router,  # type: ignore[arg-type]
    )

    result = await service.notify_result(
        from_peer="sender-codex", to_peer="target-codex", text="heads up",
    )
    get_peer.assert_not_awaited()
    assert result.reason == "broker_accepted"


@pytest.mark.asyncio
async def test_broadcast_defers_pending_peer_until_seed_settles():
    """A broadcast must not inject into a still-seeding (pending_first_turn) WS
    peer synchronously: that recipient is split out and delivered via a
    background task after the seed gate clears. Non-pending peers receive
    immediately and are never blocked by the pending one."""
    import asyncio
    from unittest.mock import AsyncMock, patch

    from repowire.daemon.peer_delivery import PeerDeliveryService

    sender = SimpleNamespace(
        peer_id="sender-id", display_name="sender", circle="c1",
        bypasses_circles=False, status=PeerStatus.ONLINE, turn_state="idle",
    )
    ready = SimpleNamespace(
        peer_id="ready-id", display_name="ready", circle="c1",
        bypasses_circles=False, status=PeerStatus.ONLINE, turn_state="idle",
    )
    pending = SimpleNamespace(
        peer_id="pending-id", display_name="pending", circle="c1",
        bypasses_circles=False, status=PeerStatus.ONLINE,
        turn_state="pending_first_turn",
    )
    settled = SimpleNamespace(
        peer_id="pending-id", display_name="pending", circle="c1",
        bypasses_circles=False, status=PeerStatus.ONLINE, turn_state="idle",
    )

    # get_peer: sender lookup + the gate's polling of the pending peer.
    by_name = {"sender": sender}
    pending_snaps = [pending, settled]
    poll = {"n": 0}

    async def get_peer(identifier):
        if identifier in by_name:
            return by_name[identifier]
        # gate polls by peer_id
        if identifier == "pending-id":
            snap = pending_snaps[min(poll["n"], len(pending_snaps) - 1)]
            poll["n"] += 1
            return snap
        return None

    registry = SimpleNamespace(
        get_peer=get_peer,
        get_all_peers=AsyncMock(return_value=[sender, ready, pending]),
        add_event=lambda *a, **k: None,
    )

    # No ACP routing for any peer (all WS).
    transport_router = SimpleNamespace(acp_route=lambda _t: None)

    ws_broadcast = AsyncMock(return_value=(["ready-id"], []))
    broadcast_to_session = AsyncMock()
    message_router = SimpleNamespace(
        broadcast=ws_broadcast,
        broadcast_to_session=broadcast_to_session,
    )

    service = PeerDeliveryService(
        registry=registry,  # type: ignore[arg-type]
        message_router=message_router,  # type: ignore[arg-type]
        transport_router=transport_router,  # type: ignore[arg-type]
    )

    import repowire.daemon.peer_delivery as pd

    with patch("repowire.daemon.seed_gate.asyncio.sleep", new_callable=AsyncMock):
        sent, failed = await service.broadcast(from_peer="sender", text="hi all")
        # Drain the deferred background task(s) to completion. Use gather on the
        # tracked task set rather than asyncio.sleep(0) — the gate's sleep is
        # patched, which would also no-op a sleep-based drain here.
        await asyncio.gather(*list(pd._DEFERRED_BROADCAST_TASKS))

    # The synchronous WS broadcast excluded the pending peer (only ready sent).
    ws_exclude = ws_broadcast.await_args.kwargs["exclude"]
    assert "pending-id" in ws_exclude
    # ready delivered synchronously; pending reported as (deferred) recipient.
    assert "ready" in sent
    assert "pending" in sent
    assert failed == []
    # The pending peer received exactly one deferred broadcast after the gate.
    broadcast_to_session.assert_awaited_once_with("sender", "hi all", "pending-id")
    assert poll["n"] >= 2  # gate re-polled pending -> settled


@pytest.mark.asyncio
async def test_query_gates_on_pending_first_turn():
    """The legacy query path also waits out the seed gate before injecting."""
    from unittest.mock import AsyncMock, patch

    from repowire.daemon.peer_delivery import PeerDeliveryService

    sender = SimpleNamespace(peer_id="sender-id", display_name="sender")
    pending = SimpleNamespace(
        peer_id="target-id", display_name="target", turn_state="pending_first_turn"
    )
    settled = SimpleNamespace(
        peer_id="target-id", display_name="target", turn_state="idle"
    )

    pending_snaps = [pending, settled]
    poll = {"n": 0}

    async def get_peer(_identifier):
        snap = pending_snaps[min(poll["n"], len(pending_snaps) - 1)]
        poll["n"] += 1
        return snap

    events = {"updated": []}
    registry = SimpleNamespace(
        check_access=AsyncMock(return_value=(sender, pending)),
        get_peer=get_peer,
        add_event=lambda *a, **k: "evt-1",
        _update_event=lambda *a, **k: events["updated"].append(a),
    )
    send_query = AsyncMock(return_value="the answer")
    transport_router = SimpleNamespace(acp_route=lambda _t: None)
    message_router = SimpleNamespace(send_query=send_query)

    service = PeerDeliveryService(
        registry=registry,  # type: ignore[arg-type]
        message_router=message_router,  # type: ignore[arg-type]
        transport_router=transport_router,  # type: ignore[arg-type]
    )

    with patch("repowire.daemon.seed_gate.asyncio.sleep", new_callable=AsyncMock):
        result = await service.query(from_peer="sender", to_peer="target", text="ping?")

    assert result == "the answer"
    send_query.assert_awaited_once()
    assert poll["n"] >= 2  # gate re-polled pending -> settled before send
