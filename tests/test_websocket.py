"""Tests for the WebSocket endpoint."""

import asyncio
import json
import os
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

    async def test_connect_advertises_capabilities(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "cappeer",
                "circle": "default",
                "backend": "claude-code",
                "path": "/tmp/cap",
                "hook_version": 1,
                "capabilities": ["delivery_receipts"],
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            session_id = resp["session_id"]

        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test"
        ) as http:
            r = await http.get(f"/peers/{session_id}")
            assert r.status_code == 200, r.text
            meta = r.json()["metadata"]
            assert meta.get("hook_version") == 1
            assert "delivery_receipts" in meta.get("capabilities", [])
        cleanup_deps()

    async def test_reconnect_preserves_http_metadata_caps_win(self, tmp_path):
        # Regression (codex judgment B): WS reconnect must NOT drop metadata an
        # HTTP SessionStart registered (project/branch); fresh WS caps win on overlap.
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test"
        ) as http:
            reg = await http.post(
                "/peers",
                json={
                    "name": "mergepeer",
                    "path": "/tmp/merge",
                    "circle": "default",
                    "backend": "claude-code",
                    "metadata": {"project": "merge", "branch": "feat/x"},
                },
            )
            assert reg.status_code == 200, reg.text
            peer_id = reg.json()["peer_id"]

        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "mergepeer",
                "circle": "default",
                "backend": "claude-code",
                "path": "/tmp/merge",
                "peer_id": peer_id,
                "hook_version": 1,
                "capabilities": ["delivery_receipts"],
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"

        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test"
        ) as http:
            r = await http.get(f"/peers/{resp['session_id']}")
            meta = r.json()["metadata"]
            assert meta.get("project") == "merge"  # HTTP metadata survived
            assert meta.get("branch") == "feat/x"
            assert "delivery_receipts" in meta.get("capabilities", [])  # fresh caps added
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
            assigned_name = resp["display_name"]
            assert assigned_name == "ws-test-claude-code"

            # Check peer list via HTTP
            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get("/peers")
                peers_list = r.json()["peers"]
                names = [p["display_name"] for p in peers_list]
                assert assigned_name in names

        cleanup_deps()

    async def test_disconnect_does_not_mark_pane_runtime_offline(self, tmp_path):
        """A dropped ws-hook socket is transport loss, not proof the pane died."""
        app = _make_app(tmp_path)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            reg = await c.post("/peers", json={
                "name": "codexpeer",
                "path": "/tmp/codexpeer",
                "circle": "default",
                "backend": "codex",
                "pane_id": "%codex",
                "agent_pid": os.getpid(),
                "metadata": {"hook_session_id": "codex-session"},
            })
            peer_id = reg.json()["peer_id"]

        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client:
            async with aconnect_ws("/ws", client) as ws:
                await ws.send_json({
                    "type": "connect",
                    "display_name": "codexpeer",
                    "circle": "default",
                    "backend": "codex",
                    "path": "/tmp/codexpeer",
                    "pane_id": "%codex",
                    "peer_id": peer_id,
                })
                resp = json.loads(await ws.receive_text())
                assert resp["type"] == "connected"
                assigned_name = resp["display_name"]
                await ws.send_json({
                    "type": "status",
                    "status": "busy",
                    "turn_state": "working",
                })

            await asyncio.sleep(0.1)

            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get(f"/peers/{assigned_name}")
                body = r.json()
                assert body["status"] == "busy"
                assert body["turn_state"] == "working"

                r = await c.post("/notify", json={
                    "from_peer": "cli",
                    "to_peer": assigned_name,
                    "text": "still explicit",
                })
                assert r.status_code == 503
                assert "no live connection" in r.json()["detail"]

        cleanup_deps()

    async def test_ws_connect_revives_http_pane_preregistration(self, tmp_path):
        app = _make_app(tmp_path)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            reg = await c.post("/peers", json={
                "name": "prepeer",
                "path": "/tmp/prepeer",
                "circle": "default",
                "backend": "codex",
                "pane_id": "%pre",
                "metadata": {"hook_session_id": "pre-session"},
            })
            assert reg.status_code == 200
            peer_id = reg.json()["peer_id"]
            assigned_name = reg.json()["display_name"]

            r = await c.get(f"/peers/{peer_id}")
            assert r.json()["status"] == "offline"

        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "prepeer",
                "circle": "default",
                "backend": "codex",
                "path": "/tmp/prepeer",
                "pane_id": "%pre",
                "peer_id": peer_id,
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            assert resp["session_id"] == peer_id

            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get(f"/peers/{assigned_name}")
                assert r.json()["status"] == "online"

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
                "path": "/tmp/statuspeer",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            assigned_name = resp["display_name"]

            # Send status update
            await ws.send_json({"type": "status", "status": "busy"})

            import asyncio
            await asyncio.sleep(0.1)  # let status propagate

            # Verify via HTTP
            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get(f"/peers/{assigned_name}")
                assert r.json()["status"] == "busy"

        cleanup_deps()

    async def test_status_update_accepts_turn_state(self, tmp_path):
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "statepeer",
                "circle": "default",
                "backend": "opencode",
                "path": "/tmp/statepeer",
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            assigned_name = resp["display_name"]

            await ws.send_json({
                "type": "status",
                "status": "busy",
                "turn_state": "working",
            })

            import asyncio
            await asyncio.sleep(0.1)

            t = ASGITransport(app=app)
            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get(f"/peers/{assigned_name}")
                body = r.json()
                assert body["status"] == "busy"
                assert body["turn_state"] == "working"

        cleanup_deps()
