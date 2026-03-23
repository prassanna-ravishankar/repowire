"""Tests for daemon authentication middleware."""

from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import Depends, FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config, DaemonConfig
from repowire.daemon.auth import require_auth, require_localhost
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_app(tmp_path: Path, auth_token: str | None = None):
    """Build a minimal app with auth-protected endpoints."""
    cfg = Config(daemon=DaemonConfig(auth_token=auth_token))
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg, message_router=router,
        query_tracker=tracker, transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()

    app_state = SimpleNamespace(
        config=cfg, transport=transport,
        query_tracker=tracker, message_router=router,
        peer_registry=registry, relay_mode=False,
    )
    init_deps(cfg, registry, app_state)

    app = FastAPI()

    @app.get("/authed")
    async def authed_endpoint(_: str | None = Depends(require_auth)):
        return {"ok": True}

    @app.get("/localhost-only")
    async def localhost_endpoint(_: None = Depends(require_localhost)):
        return {"ok": True}

    return app


class TestAuthDisabled:
    """When no auth_token is configured, endpoints are open."""

    @pytest.fixture
    async def client(self, tmp_path):
        app = _make_app(tmp_path, auth_token=None)
        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            yield c
        cleanup_deps()

    async def test_no_auth_required(self, client):
        r = await client.get("/authed")
        assert r.status_code == 200


class TestAuthEnabled:
    """When auth_token is set, bearer token is required."""

    @pytest.fixture
    async def client(self, tmp_path):
        app = _make_app(tmp_path, auth_token="secret-token-123")
        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            yield c
        cleanup_deps()

    async def test_missing_token_401(self, client):
        r = await client.get("/authed")
        assert r.status_code == 401

    async def test_wrong_token_401(self, client):
        r = await client.get("/authed", headers={"Authorization": "Bearer wrong"})
        assert r.status_code == 401

    async def test_valid_token_200(self, client):
        r = await client.get("/authed", headers={"Authorization": "Bearer secret-token-123"})
        assert r.status_code == 200


class TestRequireLocalhost:
    @pytest.fixture
    async def client(self, tmp_path):
        app = _make_app(tmp_path)
        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            yield c
        cleanup_deps()

    async def test_localhost_allowed(self, client):
        # httpx ASGITransport uses 127.0.0.1 by default
        r = await client.get("/localhost-only")
        assert r.status_code == 200
