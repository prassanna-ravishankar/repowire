"""Tests for AGENTS.md (agents_context) feature."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from click.testing import CliRunner
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.cli import main as cli_main
from repowire.config.models import Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.hooks.session_handler import _read_agents_context, _register_peer_http
from repowire.protocol.peers import Peer, PeerRole, PeerStatus
from repowire.config.models import AgentType


# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------


def _make_test_app(tmp_path: Path):
    """Build minimal app with deps initialized."""
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
    return app, registry


@pytest.fixture
async def client(tmp_path):
    app, _ = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c
    cleanup_deps()


@pytest.fixture
async def client_and_registry(tmp_path):
    app, registry = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c, registry
    cleanup_deps()


# ---------------------------------------------------------------------------
# _read_agents_context
# ---------------------------------------------------------------------------


class TestReadAgentsContext:
    def test_returns_content_when_file_exists(self, tmp_path):
        agents_md = tmp_path / "AGENTS.md"
        agents_md.write_text("# My Project\n\nDoes cool things.", encoding="utf-8")
        assert _read_agents_context(str(tmp_path)) == "# My Project\n\nDoes cool things."

    def test_returns_empty_when_no_file(self, tmp_path):
        assert _read_agents_context(str(tmp_path)) == ""

    def test_returns_empty_on_os_error(self, tmp_path):
        agents_md = tmp_path / "AGENTS.md"
        agents_md.write_text("content")
        with patch("pathlib.Path.read_text", side_effect=OSError("permission denied")):
            assert _read_agents_context(str(tmp_path)) == ""


# ---------------------------------------------------------------------------
# Peer model
# ---------------------------------------------------------------------------


class TestPeerModel:
    def test_agents_context_defaults_to_empty(self):
        peer = Peer(
            peer_id="repow-dev-abc123",
            display_name="myproject-claude-code",
            path="/tmp/myproject",
            machine="localhost",
        )
        assert peer.agents_context == ""

    def test_agents_context_stored_and_serialized(self):
        peer = Peer(
            peer_id="repow-dev-abc123",
            display_name="myproject-claude-code",
            path="/tmp/myproject",
            machine="localhost",
            agents_context="# MyProject\n\nDoes cool things.",
        )
        assert peer.agents_context == "# MyProject\n\nDoes cool things."
        d = peer.to_dict()
        assert d["agents_context"] == "# MyProject\n\nDoes cool things."


# ---------------------------------------------------------------------------
# PeerRegistry — allocate_and_register with agents_context
# ---------------------------------------------------------------------------


class TestPeerRegistryAgentsContext:
    async def test_agents_context_stored_on_register(self, tmp_path):
        cfg = Config()
        transport = WebSocketTransport()
        tracker = QueryTracker()
        router = MessageRouter(transport=transport, query_tracker=tracker)
        registry = PeerRegistry(
            config=cfg,
            message_router=router,
            persistence_path=tmp_path / "sessions.json",
        )

        peer_id, _ = await registry.allocate_and_register(
            circle="dev",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/myproject",
            agents_context="# MyProject\n\nDoes cool things.",
        )

        peer = await registry.get_peer(peer_id)
        assert peer is not None
        assert peer.agents_context == "# MyProject\n\nDoes cool things."

    async def test_agents_context_defaults_empty(self, tmp_path):
        cfg = Config()
        transport = WebSocketTransport()
        tracker = QueryTracker()
        router = MessageRouter(transport=transport, query_tracker=tracker)
        registry = PeerRegistry(
            config=cfg,
            message_router=router,
            persistence_path=tmp_path / "sessions.json",
        )

        peer_id, _ = await registry.allocate_and_register(
            circle="dev",
            backend=AgentType.CLAUDE_CODE,
            path="/tmp/myproject",
        )

        peer = await registry.get_peer(peer_id)
        assert peer is not None
        assert peer.agents_context == ""


# ---------------------------------------------------------------------------
# HTTP routes
# ---------------------------------------------------------------------------


class TestRegisterWithAgentsContext:
    async def test_register_with_agents_context(self, client):
        r = await client.post("/peers", json={
            "name": "myproject",
            "path": "/tmp/myproject",
            "circle": "dev",
            "backend": "claude-code",
            "agents_context": "# MyProject\n\nDoes cool things.",
        })
        assert r.status_code == 200
        data = r.json()
        assert data["ok"] is True
        assert "display_name" in data

    async def test_register_without_agents_context(self, client):
        r = await client.post("/peers", json={
            "name": "myproject",
            "path": "/tmp/myproject",
            "circle": "dev",
            "backend": "claude-code",
        })
        assert r.status_code == 200


class TestGetPeerContext:
    async def test_get_context_returns_agents_context(self, client_and_registry):
        client, registry = client_and_registry

        # Register a peer with agents_context
        r = await client.post("/peers", json={
            "name": "myproject",
            "path": "/tmp/myproject",
            "circle": "dev",
            "backend": "claude-code",
            "agents_context": "# MyProject\n\nDoes cool things.",
        })
        assert r.status_code == 200
        display_name = r.json()["display_name"]

        # Fetch context via the new endpoint
        r2 = await client.get(f"/peers/{display_name}/context")
        assert r2.status_code == 200
        data = r2.json()
        assert data["agents_context"] == "# MyProject\n\nDoes cool things."
        assert data["display_name"] == display_name

    async def test_get_context_returns_empty_when_no_agents_md(self, client_and_registry):
        client, _ = client_and_registry

        r = await client.post("/peers", json={
            "name": "noctx",
            "path": "/tmp/noctx",
            "circle": "dev",
            "backend": "claude-code",
        })
        display_name = r.json()["display_name"]

        r2 = await client.get(f"/peers/{display_name}/context")
        assert r2.status_code == 200
        assert r2.json()["agents_context"] == ""

    async def test_get_context_404_for_unknown_peer(self, client):
        r = await client.get("/peers/no-such-peer/context")
        assert r.status_code == 404


# ---------------------------------------------------------------------------
# Session handler — _register_peer_http reads AGENTS.md
# ---------------------------------------------------------------------------


class TestSessionHandlerAgentsContext:
    def test_register_includes_agents_context(self, tmp_path):
        agents_md = tmp_path / "AGENTS.md"
        agents_md.write_text("# MyProject\n\nDoes cool things.", encoding="utf-8")

        captured = {}

        def mock_daemon_post(path, payload):
            captured["payload"] = payload
            return {"display_name": "myproject-claude-code"}

        with patch("repowire.hooks.session_handler.daemon_post", side_effect=mock_daemon_post):
            result = _register_peer_http(str(tmp_path), "dev", AgentType.CLAUDE_CODE)

        assert result == "myproject-claude-code"
        assert captured["payload"]["agents_context"] == "# MyProject\n\nDoes cool things."

    def test_register_sends_empty_when_no_agents_md(self, tmp_path):
        captured = {}

        def mock_daemon_post(path, payload):
            captured["payload"] = payload
            return {"display_name": "myproject-claude-code"}

        with patch("repowire.hooks.session_handler.daemon_post", side_effect=mock_daemon_post):
            _register_peer_http(str(tmp_path), "dev", AgentType.CLAUDE_CODE)

        assert captured["payload"]["agents_context"] == ""


# ---------------------------------------------------------------------------
# repowire init CLI command
# ---------------------------------------------------------------------------


class TestInitCommand:
    def test_creates_agents_md(self, tmp_path):
        runner = CliRunner()
        result = runner.invoke(cli_main, ["init", "--path", str(tmp_path)])
        assert result.exit_code == 0
        agents_md = tmp_path / "AGENTS.md"
        assert agents_md.exists()
        content = agents_md.read_text()
        assert "AGENTS.md" in content
        assert tmp_path.name in content

    def test_does_not_overwrite_without_force(self, tmp_path):
        agents_md = tmp_path / "AGENTS.md"
        agents_md.write_text("existing content")
        runner = CliRunner()
        result = runner.invoke(cli_main, ["init", "--path", str(tmp_path)])
        assert result.exit_code == 0
        assert agents_md.read_text() == "existing content"

    def test_overwrites_with_force(self, tmp_path):
        agents_md = tmp_path / "AGENTS.md"
        agents_md.write_text("existing content")
        runner = CliRunner()
        result = runner.invoke(cli_main, ["init", "--path", str(tmp_path), "--force"])
        assert result.exit_code == 0
        assert agents_md.read_text() != "existing content"

    def test_includes_tech_stack_from_pyproject(self, tmp_path):
        pyproject = tmp_path / "pyproject.toml"
        pyproject.write_text(
            '[project]\nname = "myapp"\ndescription = "A test app"\ndependencies = ["fastapi>=0.100"]'
        )
        runner = CliRunner()
        runner.invoke(cli_main, ["init", "--path", str(tmp_path)])
        content = (tmp_path / "AGENTS.md").read_text()
        assert "Python" in content
        assert "FastAPI" in content
        assert "A test app" in content

    def test_includes_tech_stack_from_package_json(self, tmp_path):
        pkg = tmp_path / "package.json"
        pkg.write_text(json.dumps({
            "name": "myapp",
            "description": "A frontend app",
            "dependencies": {"next": "^14.0", "react": "^18.0"},
        }))
        runner = CliRunner()
        runner.invoke(cli_main, ["init", "--path", str(tmp_path)])
        content = (tmp_path / "AGENTS.md").read_text()
        assert "Node.js" in content
        assert "Next.js" in content
        assert "A frontend app" in content
