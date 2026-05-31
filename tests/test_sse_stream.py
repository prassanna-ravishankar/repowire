"""SSE event-driven push tests.

Validates the event-driven path replacing the old 0.5s poll loop:
  - add_event() wakes a subscribed waiter within ~10ms
  - every subscriber is woken (no single-consumer race)
  - GET /events?since=<id> returns the gap-recovery slice

The streaming endpoint itself is exercised via the registry primitives
(subscribe_events / events_since) rather than the HTTP stream, because
httpx's ASGITransport does not reliably surface intermediate SSE chunks
to the client before the generator completes.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.deps import init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import messages
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_registry(tmp_path: Path) -> PeerRegistry:
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
    return registry


def _make_app(tmp_path: Path) -> tuple[FastAPI, PeerRegistry]:
    registry = _make_registry(tmp_path)
    cfg = Config()
    app_state = SimpleNamespace(
        config=cfg,
        transport=registry._transport,
        query_tracker=registry._query_tracker,
        message_router=registry._router,
        peer_registry=registry,
        relay_mode=cfg.relay.enabled,
    )
    init_deps(cfg, registry, app_state)
    app = FastAPI()
    app.include_router(messages.router)
    return app, registry


@pytest.mark.asyncio
async def test_add_event_wakes_subscriber_within_10ms(tmp_path):
    """add_event() must wake a subscribed Event within ~10ms (not the old 500ms)."""
    registry = _make_registry(tmp_path)
    wakeup = registry.subscribe_events()
    try:
        loop = asyncio.get_event_loop()

        async def fire():
            await asyncio.sleep(0.005)
            registry.add_event("wake_test", {"payload": "ping"})

        fire_task = asyncio.create_task(fire())
        start = loop.time()
        await asyncio.wait_for(wakeup.wait(), timeout=1.0)
        elapsed = loop.time() - start
        await fire_task

        # Old behavior bound was 500ms; event-driven should be < 50ms.
        assert elapsed < 0.05, f"wake took {elapsed:.4f}s — slower than event-driven"

        new = registry.events_since(None)
        assert new and new[-1]["type"] == "wake_test"
    finally:
        registry.unsubscribe_events(wakeup)


@pytest.mark.asyncio
async def test_add_event_fans_out_to_all_subscribers(tmp_path):
    """Every subscriber gets woken — confirms no single-consumer race."""
    registry = _make_registry(tmp_path)
    a = registry.subscribe_events()
    b = registry.subscribe_events()
    c = registry.subscribe_events()
    try:
        registry.add_event("fanout", {})
        # All three should be set without blocking.
        await asyncio.wait_for(asyncio.gather(a.wait(), b.wait(), c.wait()), timeout=0.5)
        assert a.is_set() and b.is_set() and c.is_set()
    finally:
        registry.unsubscribe_events(a)
        registry.unsubscribe_events(b)
        registry.unsubscribe_events(c)


@pytest.mark.asyncio
async def test_unsubscribe_stops_wakeups(tmp_path):
    """After unsubscribe, add_event does not set the (detached) Event."""
    registry = _make_registry(tmp_path)
    wakeup = registry.subscribe_events()
    registry.unsubscribe_events(wakeup)

    registry.add_event("post_unsub", {})
    assert not wakeup.is_set()


@pytest.mark.asyncio
async def test_events_since_slice(tmp_path):
    """events_since returns the correct slice and falls back on unknown id."""
    registry = _make_registry(tmp_path)
    id_a = registry.add_event("a", {})
    id_b = registry.add_event("b", {})
    id_c = registry.add_event("c", {})

    assert [e["id"] for e in registry.events_since(id_a)] == [id_b, id_c]
    assert registry.events_since(id_c) == []
    # Unknown id → return full buffer (gap-recovery)
    full = [e["id"] for e in registry.events_since("missing")]
    assert full == [id_a, id_b, id_c]
    # None → full buffer
    assert [e["id"] for e in registry.events_since(None)] == [id_a, id_b, id_c]


@pytest.mark.asyncio
async def test_get_events_since_query_param(tmp_path):
    """GET /events?since=<id> exposes events_since over HTTP."""
    app, registry = _make_app(tmp_path)
    id_a = registry.add_event("a", {})
    id_b = registry.add_event("b", {})

    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        resp = await client.get(f"/events?since={id_a}")
        assert resp.status_code == 200
        assert [e["id"] for e in resp.json()] == [id_b]

        resp = await client.get("/events?since=does-not-exist")
        assert [e["id"] for e in resp.json()] == [id_a, id_b]

        resp = await client.get("/events")
        assert [e["id"] for e in resp.json()] == [id_a, id_b]
