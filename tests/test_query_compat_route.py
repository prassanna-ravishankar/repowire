"""Compatibility tests for the legacy /query shim."""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock

import pytest

from repowire.daemon.deps import cleanup_deps
from repowire.daemon.routes import asks, messages, peers

from .conftest import async_client_for, make_daemon_app

ROUTERS = (peers.router, messages.router, asks.router)


@pytest.fixture
async def env(tmp_path):
    harness = make_daemon_app(tmp_path, ROUTERS)
    harness.message_router.send_ask = AsyncMock()
    harness.message_router.send_notification = AsyncMock()
    async with async_client_for(harness.app) as client:
        yield client, harness
    cleanup_deps()


async def _register_peer(client, name: str, pane_id: str | None = None) -> dict:
    body: dict = {
        "name": name,
        "path": f"/tmp/{name}",
        "circle": "default",
        "backend": "claude-code",
    }
    if pane_id:
        body["pane_id"] = pane_id
    response = await client.post("/peers", json=body)
    assert response.status_code == 200, response.text
    return response.json()


async def test_query_shim_blocks_on_structured_ask_answer(env):
    client, harness = env
    target = await _register_peer(client, "target", pane_id="%42")

    query_task = asyncio.create_task(
        client.post(
            "/query",
            json={
                "to_peer": target["display_name"],
                "text": "status?",
                "timeout": 2,
            },
        )
    )
    try:
        cid = None
        for _ in range(40):
            pending = await harness.ask_tracker.pending_for_peer(target["peer_id"])
            if pending:
                cid = pending[0].correlation_id
                break
            await asyncio.sleep(0.05)
        assert cid is not None
        ask = await harness.ask_tracker.get(cid)
        assert ask is not None
        assert ask.question is not None
        assert ask.question.kind == "text"
        assert ask.question.blocking is True
        assert ask.question.metadata == {"compat": "query"}

        ack = await client.post(
            "/ack",
            json={"correlation_id": cid, "message": "all green"},
        )
        assert ack.status_code == 200, ack.text

        response = await query_task
    finally:
        if not query_task.done():
            query_task.cancel()

    assert response.status_code == 200, response.text
    assert response.json() == {"text": "all green", "error": None, "status": None}
    events = harness.registry.get_events()
    query_event = next(event for event in events if event["type"] == "query")
    response_event = next(event for event in events if event["type"] == "response")
    assert query_event["status"] == "success"
    assert query_event["to_peer_id"] == target["peer_id"]
    assert response_event["text"] == "all green"
    assert response_event["correlation_id"] == query_event["id"]


async def test_query_shim_unknown_peer_keeps_legacy_error_shape(env):
    client, _ = env

    response = await client.post(
        "/query",
        json={"to_peer": "ghost", "text": "status?", "timeout": 1},
    )

    assert response.status_code == 200, response.text
    assert response.json() == {
        "text": None,
        "error": "Unknown peer: ghost",
        "status": None,
    }
