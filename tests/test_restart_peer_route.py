"""HTTP route tests for POST /peers/{name}/restart."""

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
from repowire.protocol.peers import PeerRole
from repowire.spawn import SpawnResult


def _make_test_app(tmp_path: Path):
    cfg = Config()
    cfg.daemon.spawn.commands = {
        AgentType.CLAUDE_CODE: "claude",
        AgentType.CODEX: "codex",
    }
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
        ask_tracker=ask_tracker,
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
    spawn_routes._SPAWNED_PANE_IDS.clear()
    app, registry, ask_tracker = _make_test_app(tmp_path)
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        yield SimpleNamespace(client=client, registry=registry, ask_tracker=ask_tracker)
    spawn_routes._SPAWNED_PANE_IDS.clear()
    cleanup_deps()


async def _register(
    client: AsyncClient,
    *,
    path: str,
    backend: str = "claude-code",
    role: str = "agent",
    pane_id: str = "%101",
    machine: str | None = None,
) -> str:
    response = await client.post(
        "/peers",
        json={
            "name": Path(path).name,
            "path": path,
            "circle": "default",
            "backend": backend,
            "machine": machine or socket.gethostname(),
            "role": role,
            "pane_id": pane_id,
        },
    )
    assert response.status_code == 200, response.text
    return response.json()["display_name"]


def _spawn_result(pane_id: str = "%202") -> SpawnResult:
    return SpawnResult(
        display_name="proj",
        tmux_session="default:proj",
        pane_id=pane_id,
    )


class TestRestartPeerRoute:
    async def test_daemon_owned_success_preserves_peer_id_and_role(self, env, tmp_path):
        name = await _register(
            env.client,
            path=str(tmp_path),
            role=PeerRole.ORCHESTRATOR.value,
            pane_id="%101",
        )
        peer = await env.registry.get_peer(name)
        assert peer is not None
        spawn_routes._SPAWNED_PANE_IDS.add("%101")

        with patch.object(spawn_routes, "kill_pane", return_value=True) as mock_kill, \
            patch.object(spawn_routes, "spawn_peer", return_value=_spawn_result()) as mock_spawn, \
            patch.object(spawn_routes, "post_spawn_warmup", new_callable=AsyncMock):
            response = await env.client.post(
                f"/peers/{name}/restart",
                json={"message": "reload context"},
            )

        assert response.status_code == 200, response.text
        body = response.json()
        assert body["status"] == "restarted"
        assert body["restarted"] is True
        assert body["peer_id"] == peer.peer_id
        assert body["display_name"] == peer.display_name
        assert body["resume_mode"] == "fresh_runtime_context"
        mock_kill.assert_called_once_with("%101")
        spawn_cfg = mock_spawn.call_args.args[0]
        assert spawn_cfg.peer_id == peer.peer_id
        assert spawn_cfg.backend is AgentType.CLAUDE_CODE
        assert spawn_cfg.path == str(tmp_path.resolve())
        assert spawn_cfg.role == PeerRole.ORCHESTRATOR.value
        assert spawn_cfg.message == "reload context"
        assert "%101" not in spawn_routes._SPAWNED_PANE_IDS
        assert "%202" in spawn_routes._SPAWNED_PANE_IDS
        restarted_peer = await env.registry.get_peer(peer.peer_id)
        assert restarted_peer is not None
        assert restarted_peer.status.value == "offline"

    async def test_dry_run_reports_available_without_kill_or_spawn(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")

        with patch.object(spawn_routes, "kill_pane") as mock_kill, \
            patch.object(spawn_routes, "spawn_peer") as mock_spawn:
            response = await env.client.post(
                f"/peers/{name}/restart",
                json={"dry_run": True},
            )

        assert response.status_code == 200, response.text
        assert response.json()["status"] == "restart_available"
        assert response.json()["restarted"] is False
        mock_kill.assert_not_called()
        mock_spawn.assert_not_called()

    async def test_non_daemon_owned_pane_is_refused(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%external")

        with patch.object(spawn_routes, "kill_pane") as mock_kill, \
            patch.object(spawn_routes, "spawn_peer") as mock_spawn:
            response = await env.client.post(f"/peers/{name}/restart", json={})

        assert response.status_code == 409
        assert response.json()["detail"]["error"] == "unsupported_pane_ownership"
        mock_kill.assert_not_called()
        mock_spawn.assert_not_called()
        assert await env.registry.get_peer(name) is not None

    async def test_open_ask_blocks_restart(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        peer = await env.registry.get_peer(name)
        assert peer is not None
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        cid = await env.ask_tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text="still working?",
        )

        with patch.object(spawn_routes, "kill_pane") as mock_kill, \
            patch.object(spawn_routes, "spawn_peer") as mock_spawn:
            response = await env.client.post(f"/peers/{name}/restart", json={})

        assert response.status_code == 409
        detail = response.json()["detail"]
        assert detail["error"] == "in_flight_asks"
        assert cid in detail["open_asks"]
        mock_kill.assert_not_called()
        mock_spawn.assert_not_called()
        assert await env.registry.get_peer(name) is not None

    async def test_spawn_failure_releases_quiesce_and_old_pane_ownership(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        peer = await env.registry.get_peer(name)
        assert peer is not None
        spawn_routes._SPAWNED_PANE_IDS.add("%101")

        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "spawn_peer", side_effect=RuntimeError("tmux failed")):
            response = await env.client.post(f"/peers/{name}/restart", json={})

        assert response.status_code == 500
        assert "tmux failed" in response.text
        assert peer.peer_id not in env.ask_tracker._quiescing
        assert "%101" not in spawn_routes._SPAWNED_PANE_IDS
        kept = await env.registry.get_peer(peer.peer_id)
        assert kept is not None
        assert kept.status.value == "offline"
