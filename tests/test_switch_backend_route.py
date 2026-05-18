"""HTTP route tests for POST /peers/{name}/switch-backend (§4.8)."""

from __future__ import annotations

import socket
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_test_app(tmp_path: Path, allowed_commands: list[str] | None = None):
    cfg = Config()
    cfg.daemon.spawn.allowed_commands = (
        allowed_commands
        if allowed_commands is not None
        else ["claude", "codex", "gemini"]
    )
    cfg.daemon.spawn.allowed_paths = ["/"]
    transport = WebSocketTransport()
    tracker = QueryTracker()
    ask_tracker = AskTracker()
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
        ask_tracker=ask_tracker,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state)

    app = FastAPI()
    app.include_router(peers_routes.router)
    app.include_router(spawn_routes.router)
    return app, registry, ask_tracker


@pytest.fixture
async def env(tmp_path):
    app, registry, ask_tracker = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as client:
        yield SimpleNamespace(client=client, registry=registry, ask_tracker=ask_tracker)
    cleanup_deps()


async def _register(client, *, name="alpha", backend="claude-code", machine=None,
                    path="/tmp/peer"):
    machine = machine or socket.gethostname()
    r = await client.post("/peers", json={
        "name": name,
        "path": path,
        "circle": "default",
        "backend": backend,
        "machine": machine,
    })
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


def _fake_spawn(display_name: str, tmux_session: str, pane_id: str = "%99"):
    from repowire.spawn import SpawnResult
    return SpawnResult(
        display_name=display_name, tmux_session=tmux_session, pane_id=pane_id,
    )


class TestSwitchBackendRoute:
    async def test_same_host_happy_path(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        with patch.object(spawn_routes, "spawn_peer", return_value=_fake_spawn(
            "alpha", "default:alpha", "%101",
        )) as mock_spawn, \
            patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "post_spawn_warmup", new_callable=AsyncMock):
            r = await env.client.post(
                f"/peers/{name}/switch-backend",
                json={"new_backend": "codex"},
            )
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["ok"] is True
        assert body["old_backend"] == "claude-code"
        assert body["new_backend"] == "codex"
        assert body["display_name"] == "alpha"
        assert body["tmux_session"] == "default:alpha"

        # spawn_peer must have been invoked with the codex-mapped command and
        # the peer's original path/circle preserved.
        spawn_cfg = mock_spawn.call_args.args[0]
        assert spawn_cfg.command == "codex"
        assert spawn_cfg.backend is AgentType.CODEX
        assert spawn_cfg.circle == "default"
        assert spawn_cfg.path == str(Path(tmp_path).resolve())

        # Registry must have unregistered the old peer.
        assert await env.registry.get_peer(name) is None

    async def test_same_backend_returns_409(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        r = await env.client.post(
            f"/peers/{name}/switch-backend",
            json={"new_backend": "claude-code"},
        )
        assert r.status_code == 409
        assert r.json()["detail"]["error"] == "same_backend"

    async def test_missing_command_returns_422(self, tmp_path):
        # Build an app whose allowed_commands has no entry for gemini.
        app, _registry, _ask_tracker = _make_test_app(
            tmp_path, allowed_commands=["claude", "codex"],
        )
        try:
            transport = ASGITransport(app=app)
            async with AsyncClient(transport=transport, base_url="http://test") as client:
                name = await _register(client, name="alpha", backend="claude-code",
                                        path=str(tmp_path))
                r = await client.post(
                    f"/peers/{name}/switch-backend",
                    json={"new_backend": "gemini"},
                )
            assert r.status_code == 422
            body = r.json()
            assert body["detail"]["error"] == "command_unavailable"
            assert "allowed_commands" in body["detail"]["hint"]
            assert body["detail"]["new_backend"] == "gemini"
        finally:
            cleanup_deps()

    async def test_cross_host_returns_409(self, env, tmp_path):
        name = await _register(
            env.client, name="alpha", backend="claude-code",
            machine="other-host.example", path=str(tmp_path),
        )
        r = await env.client.post(
            f"/peers/{name}/switch-backend",
            json={"new_backend": "codex"},
        )
        assert r.status_code == 409
        assert r.json()["detail"]["error"] == "cross_host"

    async def test_in_flight_ask_blocks_switch(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        peer = await env.registry.get_peer(name)
        assert peer is not None

        # Register an open ask targeting this peer.
        await env.ask_tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text="hello",
        )

        with patch.object(spawn_routes, "spawn_peer") as mock_spawn:
            r = await env.client.post(
                f"/peers/{name}/switch-backend",
                json={"new_backend": "codex"},
            )
        assert r.status_code == 409
        detail = r.json()["detail"]
        assert detail["error"] == "in_flight_asks"
        assert len(detail["open_asks"]) == 1
        mock_spawn.assert_not_called()
        # Peer must still exist (no kill on best-effort failure path).
        assert await env.registry.get_peer(name) is not None

    async def test_unknown_peer_returns_404(self, env):
        r = await env.client.post(
            "/peers/nonexistent/switch-backend",
            json={"new_backend": "codex"},
        )
        assert r.status_code == 404


class TestCommandForBackend:
    def test_returns_first_matching_allowed_command(self, tmp_path):
        _app, _r, _a = _make_test_app(
            tmp_path,
            allowed_commands=["claude --dangerously-skip-permissions", "codex"],
        )
        try:
            assert (
                spawn_routes._command_for_backend(AgentType.CLAUDE_CODE)
                == "claude --dangerously-skip-permissions"
            )
            assert spawn_routes._command_for_backend(AgentType.CODEX) == "codex"
            assert spawn_routes._command_for_backend(AgentType.GEMINI) is None
        finally:
            cleanup_deps()
