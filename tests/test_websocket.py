"""Tests for the WebSocket endpoint."""

import json
from pathlib import Path
from types import SimpleNamespace

from httpx import ASGITransport, AsyncClient
from httpx_ws import aconnect_ws
from httpx_ws.transport import ASGIWebSocketTransport

from repowire.config.models import Config, DaemonConfig
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import health, messages, peers, websocket
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_app(tmp_path: Path, auth_token: str | None = None):
    """Build app with WebSocket endpoint."""
    cfg = Config(daemon=DaemonConfig(auth_token=auth_token))
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

    from fastapi import FastAPI

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
    app.include_router(websocket.router)
    app.include_router(spawn_routes.router)
    return app


class TestWebSocketConnect:
    async def test_connect_and_register(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "testpeer",
                "circle": "default",
                "backend": "claude-code",
                "path": "/tmp/test",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            assert "session_id" in resp
            session_id = resp["session_id"]
            assert session_id.startswith("repow-")

        cleanup_deps()

    async def test_connect_requires_display_name(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "circle": "default",
                "backend": "claude-code",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "error"

        cleanup_deps()

    async def test_connect_invalid_backend(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "test",
                "circle": "default",
                "backend": "invalid-backend",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "error"

        cleanup_deps()

    async def test_auth_required_wrong_token(self, tmp_path):
        app = _make_app(tmp_path, auth_token="secret")
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "test",
                "circle": "default",
                "backend": "claude-code",
                "auth_token": "wrong",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "error"

        cleanup_deps()

    async def test_auth_required_correct_token(self, tmp_path):
        app = _make_app(tmp_path, auth_token="secret")
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "test",
                "circle": "default",
                "backend": "claude-code",
                "auth_token": "secret",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"

        cleanup_deps()

    async def test_peer_appears_in_list_after_connect(self, tmp_path):
        app = _make_app(tmp_path)

        # Connect via WebSocket
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "wspeer",
                "circle": "default",
                "backend": "claude-code",
                "path": "/tmp/ws-test",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"

            # Check peer list via HTTP
            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get("/peers")
                peers_list = r.json()["peers"]
                names = [p["display_name"] for p in peers_list]
                assert "wspeer" in names

        cleanup_deps()

    async def test_first_message_must_be_connect(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({"type": "status", "status": "busy"})
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "error"

        cleanup_deps()


class TestWebSocketMessages:
    async def test_status_update(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "statuspeer",
                "circle": "default",
                "backend": "claude-code",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"

            # Send status update
            await ws.send_json({"type": "status", "status": "busy"})

            import asyncio
            await asyncio.sleep(0.1)  # let status propagate

            # Verify via HTTP
            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get("/peers/statuspeer")
                assert r.json()["status"] == "busy"

        cleanup_deps()
