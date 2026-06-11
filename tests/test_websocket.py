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
from repowire.daemon.deps import cleanup_deps, get_peer_registry, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import health, messages, peers, websocket
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.queued_deliveries import SQLiteQueuedDeliveryStore
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_app(tmp_path: Path, auth_token: str | None = None, *, with_queue: bool = False):
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
    state_db = StateDatabase(tmp_path / "state.db") if with_queue else None
    queue = (
        SQLiteQueuedDeliveryStore(
            state_db,
            ttl_seconds=cfg.daemon.delivery_queue_ttl_seconds,
            max_per_peer=cfg.daemon.delivery_queue_max_per_peer,
        )
        if state_db is not None
        else None
    )

    from fastapi import FastAPI

    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        message_router=router,
        peer_registry=registry,
        queued_delivery_store=queue,
        relay_mode=False,
        state_db=state_db,
    )
    init_deps(cfg, registry, app_state)

    app = FastAPI()
    if queue is not None:
        app.state.queued_delivery_store = queue
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

    async def test_retired_peer_id_claim_without_agent_pid_is_rejected(self, tmp_path):
        """An orphan ws-hook reconnecting a terminally-offlined peer is refused."""
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client:
            async with aconnect_ws("/ws", client) as ws:
                await ws.send_json({
                    "type": "connect",
                    "display_name": "zombiepeer",
                    "circle": "default",
                    "backend": "claude-code",
                    "path": "/tmp/zombie",
                })
                resp = json.loads(await ws.receive_text())
                session_id = resp["session_id"]

            registry = get_peer_registry()
            await registry.mark_offline(
                session_id, reason="agent_exited", source="test", terminal=True
            )

            async with aconnect_ws("/ws", client) as ws:
                await ws.send_json({
                    "type": "connect",
                    "display_name": "zombiepeer",
                    "circle": "default",
                    "backend": "claude-code",
                    "path": "/tmp/zombie",
                    "peer_id": session_id,
                })
                resp = json.loads(await ws.receive_text())
                assert resp["type"] == "error"
                assert resp["code"] == "peer_retired"
        cleanup_deps()

    async def test_retired_peer_id_claim_with_live_agent_pid_is_accepted(self, tmp_path):
        """A reconnect that proves a live agent reclaims the retired identity."""
        app = _make_app(tmp_path)
        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client:
            async with aconnect_ws("/ws", client) as ws:
                await ws.send_json({
                    "type": "connect",
                    "display_name": "phoenixpeer",
                    "circle": "default",
                    "backend": "claude-code",
                    "path": "/tmp/phoenix",
                })
                resp = json.loads(await ws.receive_text())
                session_id = resp["session_id"]

            registry = get_peer_registry()
            await registry.mark_offline(
                session_id, reason="session_end", source="test", terminal=True
            )

            async with aconnect_ws("/ws", client) as ws:
                await ws.send_json({
                    "type": "connect",
                    "display_name": "phoenixpeer",
                    "circle": "default",
                    "backend": "claude-code",
                    "path": "/tmp/phoenix",
                    "peer_id": session_id,
                    "agent_pid": os.getpid(),
                })
                resp = json.loads(await ws.receive_text())
                assert resp["type"] == "connected"
                assert resp["session_id"] == session_id
            assert session_id not in registry._retired
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

    async def test_ws_reconnect_reclaims_offline_same_identity_and_flushes_queue(self, tmp_path):
        app = _make_app(tmp_path, with_queue=True)

        t = ASGITransport(app=app)
        async with AsyncClient(transport=t, base_url="http://test") as c:
            reg = await c.post("/peers", json={
                "name": "orchestrator",
                "path": "/tmp/orchestrator",
                "circle": "default",
                "backend": "claude-code",
                "role": "orchestrator",
                "pane_id": "%old",
                "metadata": {"hook_session_id": "orch-runtime"},
            })
            assert reg.status_code == 200, reg.text
            peer_id = reg.json()["peer_id"]
            assigned_name = reg.json()["display_name"]
            assert assigned_name == "orchestrator-claude-code"

            state = app.state  # type: ignore[attr-defined]
            state.queued_delivery_store.enqueue(
                peer_id=peer_id,
                kind="notify",
                from_peer_name="repowire-codex",
                to_peer_name=assigned_name,
                text="[ack #ask-a4e0a845 from @repowire-codex] done",
            )
            assert state.queued_delivery_store.count_for_peer(peer_id) == 1

        async with AsyncClient(
            transport=ASGIWebSocketTransport(app), base_url="http://test"
        ) as client, aconnect_ws("/ws", client) as ws:
            await ws.send_json({
                "type": "connect",
                "display_name": "orchestrator-claude-code",
                "circle": "default",
                "backend": "claude-code",
                "path": "/tmp/orchestrator",
                "pane_id": "%7",
                "role": "orchestrator",
                "hook_version": 1,
                "capabilities": ["delivery_receipts"],
            })
            resp = json.loads(await ws.receive_text())
            assert resp["type"] == "connected"
            assert resp["session_id"] == peer_id

            notify = json.loads(await ws.receive_text())
            assert notify["type"] == "notify"
            assert notify["from_peer"] == "repowire-codex"
            assert "ask-a4e0a845" in notify["text"]
            await ws.send_json({
                "type": "delivery_ack",
                "delivery_id": notify["delivery_id"],
                "status": "injected",
            })
            await asyncio.sleep(1.0)

            async with AsyncClient(transport=t, base_url="http://test") as c:
                r = await c.get(f"/peers/{peer_id}")
                body = r.json()
                assert body["status"] == "online"
                assert body["ws_connected"] is True
                by_pane = await c.get("/peers/by-pane/%257")
                assert by_pane.status_code == 200
                by_pane_body = by_pane.json()
                assert by_pane_body["peer_id"] == peer_id
                assert by_pane_body["ws_connected"] is True
                assert by_pane_body["inbound_status"] == "online"
                assert state.queued_delivery_store.count_for_peer(peer_id) == 0

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


class TestSeedFlushGating:
    """_await_seed_settled holds the queued-notify flush until the spawn seed
    has settled (peer leaves pending_first_turn), so the seed and the flush
    don't interleave into the same pane composer."""

    @staticmethod
    def _registry(peers_by_call: list[object]):
        """Fake registry whose get_peer returns the next snapshot per call."""
        calls = {"n": 0}

        async def get_peer(_session_id: str):
            idx = min(calls["n"], len(peers_by_call) - 1)
            calls["n"] += 1
            return peers_by_call[idx]

        return SimpleNamespace(get_peer=get_peer)

    async def test_no_wait_when_not_pending_first_turn(self):
        peer = SimpleNamespace(turn_state="idle", display_name="p")
        reg = self._registry([peer])
        result = await websocket._await_seed_settled("sess", reg)
        assert result is peer

    async def test_waits_until_pending_first_turn_clears(self):
        pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
        settled = SimpleNamespace(turn_state="working", display_name="p")
        # First snapshot pending, second pending, third settled.
        reg = self._registry([pending, pending, settled])

        from unittest.mock import AsyncMock, patch

        with patch.object(websocket.asyncio, "sleep", new_callable=AsyncMock) as sleep:
            result = await websocket._await_seed_settled("sess", reg)

        assert result is settled
        assert sleep.await_count >= 1  # polled at least once while pending

    async def test_proceeds_on_timeout_while_still_pending(self):
        pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
        reg = self._registry([pending])

        from unittest.mock import AsyncMock, patch

        # Zero wait budget: the deadline check trips on the first iteration, so
        # the loop breaks and flushes anyway rather than hanging forever.
        with (
            patch.object(websocket.asyncio, "sleep", new_callable=AsyncMock),
            patch.object(websocket, "_SEED_FLUSH_WAIT_SECONDS", 0.0),
        ):
            result = await websocket._await_seed_settled("sess", reg)

        # Returns the still-pending snapshot rather than hanging forever.
        assert result is pending

    async def test_returns_none_when_peer_vanishes_mid_wait(self):
        pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
        reg = self._registry([pending, None])

        from unittest.mock import AsyncMock, patch

        with patch.object(websocket.asyncio, "sleep", new_callable=AsyncMock):
            result = await websocket._await_seed_settled("sess", reg)

        assert result is None
