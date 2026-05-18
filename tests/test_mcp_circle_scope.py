"""Tests for circle-scoped MCP peer surface (#125 slice 2).

Covers:
- list_peers defaults to caller's circle, surfaces bypass-roles always
- list_peers `circle='*'` returns mesh-wide
- list_peers for orchestrator role defaults to mesh-wide
- ask/notify_peer default circle to caller's, fall back to bypass-only
"""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from repowire.mcp import server as mcp_server
from repowire.protocol.errors import DaemonHTTPError


@pytest.fixture(autouse=True)
def reset_cache():
    mcp_server._cached_peer_name = None
    mcp_server._cached_peer_id = None
    mcp_server._cached_my_circle = None
    mcp_server._cached_my_role = None
    mcp_server._registered = True  # bypass _ensure_registered side effects
    yield
    mcp_server._cached_peer_name = None
    mcp_server._cached_peer_id = None
    mcp_server._cached_my_circle = None
    mcp_server._cached_my_role = None
    mcp_server._registered = False


def _seed_identity(name: str, circle: str, role: str, peer_id: str | None = None) -> None:
    mcp_server._cached_peer_name = name
    mcp_server._cached_peer_id = peer_id
    mcp_server._cached_my_circle = circle
    mcp_server._cached_my_role = role


def _peers_response(*display_names: str) -> dict:
    return {"peers": [{"display_name": n, "circle": "x"} for n in display_names]}


def _get_list_peers_tool():
    server = mcp_server.create_mcp_server()
    tool = server._tool_manager._tools["list_peers"]
    return tool.fn


def _get_ask_tool():
    server = mcp_server.create_mcp_server()
    tool = server._tool_manager._tools["ask"]
    return tool.fn


def _get_notify_tool():
    server = mcp_server.create_mcp_server()
    tool = server._tool_manager._tools["notify_peer"]
    return tool.fn


class TestListPeersCircleScope:
    @pytest.mark.asyncio
    async def test_identity_lookup_prefers_cached_peer_id(self):
        _seed_identity("dupe-name", "", "", peer_id="repow-team-a-me")
        mcp_server._cached_my_circle = None
        mcp_server._cached_my_role = None
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["path"] = path
            return {
                "peer_id": "repow-team-a-me",
                "display_name": "dupe-name",
                "circle": "team-a",
                "role": "agent",
            }

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            name, circle, role = await mcp_server._get_my_identity()

        assert captured["path"] == "/peers/repow-team-a-me"
        assert name == "dupe-name"
        assert circle == "team-a"
        assert role == "agent"

    @pytest.mark.asyncio
    async def test_default_scopes_to_callers_circle(self):
        _seed_identity("me", "team-a", "agent")
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers()

        assert captured["params"]["circle"] == "team-a"
        assert captured["params"]["status"] == "online"

    @pytest.mark.asyncio
    async def test_star_widens_to_mesh(self):
        _seed_identity("me", "team-a", "agent")
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params or {}
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers(circle="*")

        assert "circle" not in captured["params"]

    @pytest.mark.asyncio
    async def test_explicit_concrete_circle_overrides_default(self):
        _seed_identity("me", "team-a", "agent")
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers(circle="team-b")

        assert captured["params"]["circle"] == "team-b"

    @pytest.mark.asyncio
    async def test_orchestrator_role_defaults_to_mesh_wide(self):
        _seed_identity("orch", "default", "orchestrator")
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params or {}
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers()

        assert "circle" not in captured["params"]

    @pytest.mark.asyncio
    async def test_orchestrator_can_still_scope_explicitly(self):
        _seed_identity("orch", "default", "orchestrator")
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers(circle="team-a")

        assert captured["params"]["circle"] == "team-a"

    @pytest.mark.asyncio
    async def test_no_identity_no_circle_param(self):
        """If we can't resolve caller's circle, fall through to global lookup
        rather than dropping a bogus filter."""
        list_peers = _get_list_peers_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            captured["params"] = params or {}
            return _peers_response()

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await list_peers()

        assert "circle" not in captured["params"]


class TestAskNotifyCircleResolution:
    @pytest.mark.asyncio
    async def test_ask_uses_callers_circle_when_peer_found_locally(self):
        _seed_identity("me", "team-a", "agent")
        ask = _get_ask_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            if path.startswith("/peers/") and method == "GET":
                # caller's circle lookup hits
                return {"display_name": "bob", "circle": "team-a", "role": "agent"}
            if path == "/ask":
                captured["body"] = body
                return {"correlation_id": "ask-1"}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await ask("bob", "hi")

        assert captured["body"]["circle"] == "team-a"

    @pytest.mark.asyncio
    async def test_ask_falls_back_to_bypass_role_globally(self):
        _seed_identity("me", "team-a", "agent")
        ask = _get_ask_tool()
        captured: dict = {}
        calls: list[tuple] = []

        async def fake_request(method, path, body=None, params=None):
            calls.append((method, path, params))
            if path.startswith("/peers/") and method == "GET":
                if params and params.get("circle") == "team-a":
                    raise DaemonHTTPError(404, "not in team-a")
                # global lookup finds telegram in 'default' with role=service
                return {"display_name": "telegram", "circle": "default", "role": "service"}
            if path == "/ask":
                captured["body"] = body
                return {"correlation_id": "ask-2"}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await ask("telegram", "ping")

        # Sent with the resolved bypass-role peer's circle, not team-a.
        assert captured["body"]["circle"] == "default"

    @pytest.mark.asyncio
    async def test_ask_does_not_fall_back_to_non_bypass_peer(self):
        """Cross-circle agent must NOT be reachable via implicit fallback."""
        _seed_identity("me", "team-a", "agent")
        ask = _get_ask_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            if path.startswith("/peers/") and method == "GET":
                if params and params.get("circle") == "team-a":
                    raise DaemonHTTPError(404, "miss")
                # global lookup finds an agent in another circle
                return {"display_name": "alien", "circle": "team-b", "role": "agent"}
            if path == "/ask":
                captured["body"] = body
                return {"correlation_id": "ask-3"}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await ask("alien", "hi")

        # Forced to caller's circle so the daemon will 404 — no implicit
        # cross-circle leak.
        assert captured["body"]["circle"] == "team-a"

    @pytest.mark.asyncio
    async def test_ask_explicit_circle_passes_through(self):
        _seed_identity("me", "team-a", "agent")
        ask = _get_ask_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            if path == "/ask":
                captured["body"] = body
                return {"correlation_id": "ask-4"}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await ask("bob", "hi", circle="team-c")

        assert captured["body"]["circle"] == "team-c"

    @pytest.mark.asyncio
    async def test_ask_uses_cached_peer_id_as_sender(self):
        _seed_identity("me", "team-a", "agent", peer_id="repow-team-a-me")
        ask = _get_ask_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            if path.startswith("/peers/") and method == "GET":
                return {"display_name": "bob", "circle": "team-a", "role": "agent"}
            if path == "/ask":
                captured["body"] = body
                return {"correlation_id": "ask-5"}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await ask("bob", "hi")

        assert captured["body"]["from_peer"] == "repow-team-a-me"

    @pytest.mark.asyncio
    async def test_notify_peer_uses_same_fallback(self):
        _seed_identity("me", "team-a", "agent")
        notify_peer = _get_notify_tool()
        captured: dict = {}

        async def fake_request(method, path, body=None, params=None):
            if path.startswith("/peers/") and method == "GET":
                if params and params.get("circle") == "team-a":
                    raise DaemonHTTPError(404, "miss")
                return {"display_name": "telegram", "circle": "default", "role": "service"}
            if path == "/notify":
                captured["body"] = body
                return {}
            return {}

        with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=fake_request)):
            await notify_peer("telegram", "hello")

        assert captured["body"]["circle"] == "default"
