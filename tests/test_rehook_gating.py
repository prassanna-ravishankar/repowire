"""Gating tests for POST /peers/{name}/rehook (non-destructive recovery)."""

from __future__ import annotations

import socket
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

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


def _make_app(tmp_path: Path):
    cfg = Config()
    cfg.daemon.spawn.commands = {AgentType.CLAUDE_CODE: "claude"}
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
    return app, registry, transport


@pytest.fixture
async def env(tmp_path, monkeypatch):
    spawn_routes._SPAWNED_PANE_IDS.clear()
    monkeypatch.setattr("repowire.spawn_ownership.OWNERSHIP_PATH", tmp_path / "ownership.json")
    app, registry, transport = _make_app(tmp_path)
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        yield SimpleNamespace(client=client, registry=registry, transport=transport)
    spawn_routes._SPAWNED_PANE_IDS.clear()
    cleanup_deps()


async def _register(client, *, path: str, pane_id: str | None = "%101") -> str:
    payload = {
        "name": Path(path).name,
        "path": path,
        "circle": "default",
        "backend": "claude-code",
        "machine": socket.gethostname(),
        "role": "agent",
    }
    if pane_id is not None:
        payload["pane_id"] = pane_id
    r = await client.post("/peers", json=payload)
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


class TestRehookGating:
    async def test_dry_run_default_no_side_effects(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        with patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn:
            r = await env.client.post(f"/peers/{name}/rehook", json={})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["acted"] is False
        assert body["reason"] == "dry_run"
        mock_respawn.assert_not_called()

    async def test_ping_healthy_peer_not_disconnected(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        peer = await env.registry.get_peer(name)
        with patch.object(env.transport, "is_connected", return_value=True), \
            patch.object(env.transport, "ping", return_value={"pane_alive": True}) as mock_ping, \
            patch.object(env.transport, "disconnect") as mock_disconnect, \
            patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn:
            r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["acted"] is False
        assert body["reason"] == "already_healthy"
        assert body["ping_ok"] is True
        mock_ping.assert_awaited()
        mock_disconnect.assert_not_called()
        mock_respawn.assert_not_called()
        assert peer is not None

    async def test_pane_unverified_409(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%999")
        # NOT in _SPAWNED_PANE_IDS and probe_tmux_pane returns None => unverified.
        with patch.object(spawn_routes, "probe_tmux_pane", return_value=None), \
            patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn:
            r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 409, r.text
        assert r.json()["detail"]["error"] == "pane_unverified"
        mock_respawn.assert_not_called()

    async def test_pane_path_mismatch_rejected(self, env, tmp_path):
        # Pane exists but its current_path is NOT the peer's path, and no hook
        # metadata match -> must NOT verify (would misroute injected messages).
        from repowire.spawn_ownership import TmuxPaneEvidence

        name = await _register(env.client, path=str(tmp_path), pane_id="%555")
        wrong = TmuxPaneEvidence(
            pane_id="%555",
            tmux_session="default:other",
            current_path="/some/other/path",
            pane_pid="123",
        )
        with patch.object(spawn_routes, "probe_tmux_pane", return_value=wrong), \
            patch.object(spawn_routes, "read_pane_runtime_metadata", return_value={}), \
            patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn:
            r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 409, r.text
        assert r.json()["detail"]["error"] == "pane_unverified"
        mock_respawn.assert_not_called()

    async def test_pane_path_match_verifies(self, env, tmp_path):
        # Pane exists AND current_path matches the peer's path -> verified.
        from repowire.spawn_ownership import TmuxPaneEvidence

        name = await _register(env.client, path=str(tmp_path), pane_id="%556")
        match = TmuxPaneEvidence(
            pane_id="%556",
            tmux_session="default:proj",
            current_path=str(tmp_path),
            pane_pid="123",
        )
        with patch.object(spawn_routes, "probe_tmux_pane", return_value=match), \
            patch.object(env.transport, "is_connected", return_value=False), \
            patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn:
            r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 200, r.text
        assert r.json()["acted"] is True
        mock_respawn.assert_called_once()

    async def test_missing_pane_409(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id=None)
        r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 409, r.text
        assert r.json()["detail"]["error"] == "missing_pane"

    async def test_cross_host_409(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        peer = await env.registry.get_peer(name)
        assert peer is not None
        peer.machine = "some-other-host"
        r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 409, r.text
        assert r.json()["detail"]["error"] == "cross_host"

    async def test_apply_respawns_and_never_kills(self, env, tmp_path):
        name = await _register(env.client, path=str(tmp_path), pane_id="%101")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        # Disconnected peer: no ws connection => proceeds to respawn.
        with patch.object(env.transport, "is_connected", return_value=False), \
            patch.object(spawn_routes, "maybe_respawn", return_value=True) as mock_respawn, \
            patch.object(spawn_routes, "kill_pane") as mock_kill:
            r = await env.client.post(f"/peers/{name}/rehook", json={"apply": True})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["acted"] is True
        assert body["ws_hook_respawned"] is True
        assert body["reason"] == "respawned"
        mock_respawn.assert_called_once()
        mock_kill.assert_not_called()
