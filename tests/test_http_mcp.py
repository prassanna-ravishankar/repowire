"""Tests for the opt-in daemon Streamable HTTP MCP endpoint."""

from __future__ import annotations

import json
from pathlib import Path

import anyio
import pytest
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config, DaemonConfig
from repowire.daemon.app import create_test_app
from repowire.daemon.deps import cleanup_deps
from repowire.protocol.errors import DaemonHTTPError


@pytest.fixture(autouse=True)
def reset_mcp_context_after_http_tests():
    from repowire.mcp.server import reset_mcp_context

    reset_mcp_context()
    yield
    reset_mcp_context()


def _cfg(*, enabled: bool, token: str | None = "secret") -> Config:
    return Config(daemon=DaemonConfig(auth_token=token, mcp_http={"enabled": enabled}))


async def _client(app):
    return AsyncClient(
        transport=ASGITransport(app=app),
        base_url="http://127.0.0.1:8377",
        follow_redirects=True,
    )


class TestHttpMCPMount:
    def test_mcp_disabled_by_default(self):
        async def run():
            app = create_test_app(_cfg(enabled=False))
            async with await _client(app) as client:
                r = await client.post("/mcp")
                assert r.status_code == 404
            cleanup_deps()

        anyio.run(run)

    def test_mcp_enabled_requires_bearer_auth(self):
        async def run():
            app = create_test_app(_cfg(enabled=True))
            async with await _client(app) as client:
                r = await client.post("/mcp")
                assert r.status_code == 401
                assert r.headers["www-authenticate"] == "Bearer"
            cleanup_deps()

        anyio.run(run)

    def test_mcp_enabled_rejects_bad_token(self):
        async def run():
            app = create_test_app(_cfg(enabled=True))
            async with await _client(app) as client:
                r = await client.post("/mcp", headers={"Authorization": "Bearer wrong"})
                assert r.status_code == 401
            cleanup_deps()

        anyio.run(run)

    def test_tools_list_over_http_when_enabled_and_authenticated(self):
        async def run():
            app = create_test_app(_cfg(enabled=True))
            headers = {
                "Authorization": "Bearer secret",
                "Accept": "application/json, text/event-stream",
                "Content-Type": "application/json",
            }
            async with app.router.lifespan_context(app):
                async with await _client(app) as client:
                    init = await client.post(
                        "/mcp",
                        headers=headers,
                        json={
                            "jsonrpc": "2.0",
                            "id": 1,
                            "method": "initialize",
                            "params": {
                                "protocolVersion": "2025-03-26",
                                "capabilities": {},
                                "clientInfo": {"name": "pytest", "version": "0"},
                            },
                        },
                    )
                    assert init.status_code == 200
                    session_id = init.headers["mcp-session-id"]
                    headers["mcp-session-id"] = session_id

                    initialized = await client.post(
                        "/mcp",
                        headers=headers,
                        json={
                            "jsonrpc": "2.0",
                            "method": "notifications/initialized",
                            "params": {},
                        },
                    )
                    assert initialized.status_code in (200, 202)

                    tools = await client.post(
                        "/mcp",
                        headers=headers,
                        json={"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
                    )
                    assert tools.status_code == 200
                    payload = _sse_json(tools.text)
                    names = {tool["name"] for tool in payload["result"]["tools"]}
                    assert {"whoami", "list_peers", "ask", "ack", "notify_peer"} <= names
            cleanup_deps()

        anyio.run(run)


def _sse_json(text: str) -> dict:
    for line in text.splitlines():
        if line.startswith("data: "):
            return json.loads(line.removeprefix("data: "))
    raise AssertionError(f"No SSE data line found: {text}")


def test_http_mcp_identity_does_not_use_daemon_cwd(monkeypatch):
    """HTTP MCP identity registration must not fall back to Path.cwd()."""
    async def run():
        import repowire.mcp.server as server

        calls: list[tuple[str, str, dict | None]] = []

        async def fake_daemon_request(method: str, path: str, body=None, params=None):
            calls.append((method, path, body))
            if method == "GET":
                raise DaemonHTTPError(404, "not found")
            assert method == "POST"
            assert path == "/peers"
            return {
                "peer_id": "repow-global-http1234",
                "display_name": "peer-mcp-http",
                "circle": body["circle"],
                "role": body["role"],
            }

        monkeypatch.setattr(server, "daemon_request", fake_daemon_request)
        monkeypatch.setattr(Path, "cwd", lambda: (_ for _ in ()).throw(AssertionError("cwd used")))

        server.configure_http_mcp_context(auth_token="secret")
        await server._ensure_registered(strict=True)

        register_body = next(
            body for method, path, body in calls if method == "POST" and path == "/peers"
        )
        assert register_body["name"] == "mcp-http"
        assert register_body["path"] == ""
        assert register_body["backend"] == "mcp-http"
        assert register_body["role"] == "human"

    anyio.run(run)
