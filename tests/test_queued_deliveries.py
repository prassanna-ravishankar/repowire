"""Tests for SQLite queued delivery fallback."""

from __future__ import annotations

import io
import json
import time
from contextlib import redirect_stdout
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_delivery import PeerDeliveryService
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import asks, messages, peers
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.queued_deliveries import SQLiteQueuedDeliveryStore
from repowire.daemon.transport_router import transport_router_from_state
from repowire.daemon.websocket_transport import TransportError, WebSocketTransport
from repowire.hooks.stop_handler import main as stop_main


def _make_app(tmp_path: Path, cfg: Config | None = None) -> tuple[FastAPI, SimpleNamespace]:
    cfg = cfg or Config()
    transport = WebSocketTransport()
    query_tracker = QueryTracker()
    ask_tracker = AskTracker(ttl_hours=24.0)
    msg_router = MessageRouter(transport=transport, query_tracker=query_tracker)
    registry = PeerRegistry(
        config=cfg,
        message_router=msg_router,
        query_tracker=query_tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
        ask_tracker=ask_tracker,
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600
    state_db = StateDatabase(tmp_path / "state.db")
    queue = SQLiteQueuedDeliveryStore(
        state_db,
        ttl_seconds=cfg.daemon.delivery_queue_ttl_seconds,
        max_per_peer=cfg.daemon.delivery_queue_max_per_peer,
    )
    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=query_tracker,
        ask_tracker=ask_tracker,
        message_router=msg_router,
        peer_registry=registry,
        queued_delivery_store=queue,
        relay_mode=False,
        state_db=state_db,
    )
    state.peer_delivery = PeerDeliveryService(
        registry=registry,
        message_router=msg_router,
        transport_router=transport_router_from_state(
            config=cfg,
            registry=registry,
            state=state,
        ),
        ask_tracker=ask_tracker,
        queued_delivery_store=queue,
    )
    init_deps(cfg, registry, state)

    msg_router.send_notification = AsyncMock()
    msg_router.send_ask = AsyncMock()

    app = FastAPI()
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(asks.router)
    return app, state


async def _register(client: AsyncClient, name: str, *, fallback: bool = False) -> dict:
    body: dict = {
        "name": name,
        "path": f"/tmp/{name}",
        "circle": "default",
        "backend": "claude-code",
    }
    if fallback:
        body["metadata"] = {"repowire_cli_fallback": True}
    response = await client.post("/peers", json=body)
    assert response.status_code == 200, response.text
    return response.json()


def test_store_expires_caps_and_drains_without_duplicates(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteQueuedDeliveryStore(db, ttl_seconds=60, max_per_peer=2)
        now = datetime(2026, 5, 25, 12, 0, tzinfo=timezone.utc)
        store.enqueue(
            peer_id="peer-1", kind="notify", from_peer_name="a", to_peer_name="b",
            text="old", now=now,
        )
        second = store.enqueue(
            peer_id="peer-1", kind="notify", from_peer_name="a", to_peer_name="b",
            text="second", now=now + timedelta(seconds=1),
        )
        third = store.enqueue(
            peer_id="peer-1", kind="notify", from_peer_name="a", to_peer_name="b",
            text="third", now=now + timedelta(seconds=2),
        )

        assert second is not None and third is not None
        assert store.count_for_peer("peer-1") == 2
        drained = store.drain_for_peer("peer-1", now=now + timedelta(seconds=3))
        assert [d.text for d in drained] == ["second", "third"]
        assert store.drain_for_peer("peer-1", now=now + timedelta(seconds=4)) == []

        store.enqueue(
            peer_id="peer-1", kind="notify", from_peer_name="a", to_peer_name="b",
            text="expires", now=now,
        )
        assert store.drain_for_peer("peer-1", now=now + timedelta(seconds=61)) == []
    finally:
        db.close()


async def test_notify_transport_failure_queues_and_drains(tmp_path: Path) -> None:
    app, state = _make_app(tmp_path)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        alice = await _register(client, "alice")
        bob = await _register(client, "bob")
        state.message_router.send_notification.side_effect = TransportError("No connection")

        response = await client.post("/notify", json={
            "from_peer": alice["display_name"],
            "to_peer": bob["display_name"],
            "text": "offline ping",
        })

        assert response.status_code == 200, response.text
        body = response.json()
        assert body["delivery_state"] == "queued"
        assert body["reason"] == "queued_delivery"
        drained = await client.get("/deliveries/pending", params={"peer_id": bob["peer_id"]})
        assert drained.status_code == 200
        assert drained.json()["deliveries"][0]["text"] == "offline ping"
        drained_again = await client.get(
            "/deliveries/pending", params={"peer_id": bob["peer_id"]},
        )
        assert drained_again.json()["deliveries"] == []
    cleanup_deps()
    state.state_db.close()


async def test_live_notify_delivery_is_not_queued(tmp_path: Path) -> None:
    app, state = _make_app(tmp_path)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        alice = await _register(client, "alice")
        bob = await _register(client, "bob")

        response = await client.post("/notify", json={
            "from_peer": alice["display_name"],
            "to_peer": bob["display_name"],
            "text": "live ping",
        })

        assert response.status_code == 200, response.text
        assert response.json()["delivery_state"] == "delivered"
        assert state.queued_delivery_store.count_for_peer(bob["peer_id"]) == 0
    cleanup_deps()
    state.state_db.close()


async def test_cli_fallback_ask_is_queued_once_and_still_pending(tmp_path: Path) -> None:
    app, state = _make_app(tmp_path)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        alice = await _register(client, "alice")
        agy = await _register(client, "agy", fallback=True)
        state.message_router.send_ask.side_effect = TransportError("No connection")

        response = await client.post("/ask", json={
            "from_peer": alice["display_name"],
            "to_peer": agy["display_name"],
            "text": "queued ask",
        })

        assert response.status_code == 200, response.text
        cid = response.json()["correlation_id"]
        drained = await client.get("/deliveries/pending", params={"peer_id": agy["peer_id"]})
        assert drained.json()["deliveries"][0]["correlation_id"] == cid
        assert (await client.get(
            "/deliveries/pending", params={"peer_id": agy["peer_id"]},
        )).json()["deliveries"] == []
        pending = await client.get("/asks/pending", params={"peer_id": agy["peer_id"]})
        assert pending.json()["asks"][0]["correlation_id"] == cid
    cleanup_deps()
    state.state_db.close()


def test_stop_hook_drains_queued_deliveries_before_reminders() -> None:
    buf = io.StringIO()
    with patch("sys.stdin") as stdin, redirect_stdout(buf), \
        patch("repowire.hooks.stop_handler.get_pane_id", return_value="%42"), \
        patch("repowire.hooks.stop_handler.get_display_name", return_value="bob"), \
        patch("repowire.hooks.stop_handler.update_status", return_value=True), \
        patch("repowire.hooks.stop_handler.daemon_post"), \
        patch("repowire.hooks.stop_handler.fetch_queued_deliveries", return_value=[{
            "kind": "notify",
            "from_peer": "alice",
            "text": "queued hello",
        }]), \
        patch("repowire.hooks.stop_handler.fetch_and_filter_pending", return_value=[]):
        stdin.read.return_value = json.dumps({"cwd": "/tmp/test", "session_id": "s1"})

        assert stop_main() == 0

    out = json.loads(buf.getvalue())
    assert out["decision"] == "block"
    assert "queued hello" in out["reason"]
    assert "@alice [notify]" in out["reason"]
