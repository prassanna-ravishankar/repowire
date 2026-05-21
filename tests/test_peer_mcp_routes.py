"""HTTP route tests for per-peer MCP config (#183)."""

from __future__ import annotations

import socket
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire import peer_mcp
from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, get_peer_registry, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_test_app(tmp_path: Path):
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
    return app


@pytest.fixture
async def client(tmp_path):
    app = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c
    cleanup_deps()


async def _register(client, name="local", backend="codex", machine=None):
    machine = machine or socket.gethostname()
    r = await client.post("/peers", json={
        "name": name,
        "path": "/tmp/peer",
        "circle": "default",
        "backend": backend,
        "machine": machine,
    })
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


class TestMcpRoutes:
    async def test_list_unknown_peer_404(self, client):
        r = await client.get("/peers/nonexistent/mcp")
        assert r.status_code == 404

    async def test_cross_host_rejected_409(self, client):
        name = await _register(client, name="remote", machine="other-host.example")
        r = await client.get(f"/peers/{name}/mcp")
        assert r.status_code == 409
        assert r.json()["detail"]["error"] == "cross_host"

    async def test_list_add_remove_cycle_codex(self, client, tmp_path, monkeypatch):
        # Codex backend uses on-disk config; redirect to tmp_path.
        cfg = tmp_path / "config.toml"
        monkeypatch.setattr(peer_mcp, "CODEX_CONFIG_PATH", cfg)

        name = await _register(client, name="codexlocal", backend="codex")

        r = await client.get(f"/peers/{name}/mcp")
        assert r.status_code == 200
        body = r.json()
        assert body["servers"] == []
        assert body["config_scope"]["backend"] == "codex"
        assert body["config_scope"]["effective_scope"] == "backend_global"
        assert body["config_scope"]["is_global"] is True
        assert body["config_scope"]["supported_scopes"] == ["user"]

        r = await client.post(f"/peers/{name}/mcp", json={
            "name": "repowire",
            "command": "repowire",
            "args": ["mcp"],
        })
        assert r.status_code == 200
        assert r.json()["config_scope"]["effective_scope"] == "backend_global"

        r = await client.get(f"/peers/{name}/mcp")
        assert r.status_code == 200
        servers = r.json()["servers"]
        assert len(servers) == 1
        assert servers[0]["name"] == "repowire"

        # Duplicate add returns 409
        r = await client.post(f"/peers/{name}/mcp", json={
            "name": "repowire", "command": "repowire",
        })
        assert r.status_code == 409

        # Remove
        r = await client.delete(f"/peers/{name}/mcp/repowire")
        assert r.status_code == 200
        assert r.json()["config_scope"]["backend"] == "codex"

        # Remove again -> 404
        r = await client.delete(f"/peers/{name}/mcp/repowire")
        assert r.status_code == 404

    async def test_add_invalid_name_returns_400(self, client, tmp_path, monkeypatch):
        cfg = tmp_path / "config.toml"
        monkeypatch.setattr(peer_mcp, "CODEX_CONFIG_PATH", cfg)
        name = await _register(client, name="codexlocal", backend="codex")

        r = await client.post(f"/peers/{name}/mcp", json={
            "name": "bad.name",
            "command": "repowire",
        })

        assert r.status_code == 400
        assert "server name" in r.json()["detail"]
        assert not cfg.exists()

    async def test_codex_rejects_project_scope_with_metadata(self, client, tmp_path, monkeypatch):
        cfg = tmp_path / "config.toml"
        monkeypatch.setattr(peer_mcp, "CODEX_CONFIG_PATH", cfg)

        name = await _register(client, name="codexscope", backend="codex")

        r = await client.post(f"/peers/{name}/mcp?scope=project", json={
            "name": "repowire",
            "command": "repowire",
        })
        assert r.status_code == 409
        detail = r.json()["detail"]
        assert detail["error"] == "unsupported_scope"
        assert detail["requested_scope"] == "project"
        assert detail["supported_scopes"] == ["user"]
        assert detail["config_scope"]["effective_scope"] == "backend_global"

    async def test_unsupported_backend_returns_501(self, client):
        name = await _register(client, name="pipeer", backend="claude-code")
        # Force registry to report opencode
        registry = get_peer_registry()
        peer = await registry.get_peer(name)
        assert peer is not None
        peer.backend = AgentType.OPENCODE

        r = await client.get(f"/peers/{name}/mcp")
        assert r.status_code == 501

    async def test_claude_code_timeout_returns_504(self, client):
        import subprocess as sp
        name = await _register(client, name="ccpeer", backend="claude-code")
        timeout = sp.TimeoutExpired(cmd="claude", timeout=10)
        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", side_effect=timeout):
            r = await client.get(f"/peers/{name}/mcp")
        assert r.status_code == 504
