"""Tests for the /traces/{id} route and ask/notify stage instrumentation."""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest

from repowire.daemon.deps import cleanup_deps
from repowire.daemon.routes import asks, messages, peers, traces

from .conftest import async_client_for, make_daemon_app

ROUTERS = (peers.router, asks.router, messages.router, traces.router)


@pytest.fixture
async def env(tmp_path):
    harness = make_daemon_app(tmp_path, ROUTERS)
    harness.message_router.send_notification = AsyncMock()
    harness.message_router.send_ask = AsyncMock()
    async with async_client_for(harness.app) as c:
        yield c, harness
    cleanup_deps()


async def _register(client, name: str) -> str:
    r = await client.post(
        "/peers",
        json={"name": name, "path": f"/tmp/{name}", "circle": "default", "backend": "claude-code"},
    )
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


class TestTraceRoute:
    async def test_unknown_trace_404(self, env):
        client, _ = env
        r = await client.get("/traces/ask-nope")
        assert r.status_code == 404

    async def test_seed_and_fetch_ordered(self, env):
        client, harness = env
        store = harness.delivery_trace_store
        for stage in ("created", "resolved_peer", "routed", "websocket_sent", "pane_injected"):
            store.record(trace_id="ask-seed", kind="ask", stage=stage, peer_id="p1")
        r = await client.get("/traces/ask-seed")
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["trace_id"] == "ask-seed"
        assert body["kind"] == "ask"
        assert [s["stage"] for s in body["stages"]] == [
            "created",
            "resolved_peer",
            "routed",
            "websocket_sent",
            "pane_injected",
        ]
        assert [s["seq"] for s in body["stages"]] == [0, 1, 2, 3, 4]


class TestAskStagesLand:
    async def test_successful_ask_records_stages(self, env):
        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        r = await client.post("/ask", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 200, r.text
        cid = r.json()["correlation_id"]
        # The instrumented open_ask should have recorded the success-path stages.
        rows = harness.delivery_trace_store.stages_for(cid)
        stages = [r.stage for r in rows]
        assert "created" in stages
        assert "resolved_peer" in stages
        assert "routed" in stages
        assert "pane_injected" in stages

    async def test_ack_records_closed(self, env):
        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        r = await client.post("/ask", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        cid = r.json()["correlation_id"]
        ack = await client.post("/ack", json={"correlation_id": cid})
        assert ack.status_code == 200, ack.text
        stages = [row.stage for row in harness.delivery_trace_store.stages_for(cid)]
        assert "acked" in stages
        assert "closed" in stages
