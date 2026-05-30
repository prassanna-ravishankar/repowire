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
    async def test_legacy_no_ack_does_not_record_pane_injected(self, env):
        # send_ask is mocked (no real delivery_ack), modelling a legacy hook
        # that never acks. The trace must NOT claim pane_injected — only the
        # unverified websocket_sent handoff.
        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        r = await client.post("/ask", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 200, r.text
        cid = r.json()["correlation_id"]
        rows = harness.delivery_trace_store.stages_for(cid)
        stages = [row.stage for row in rows]
        assert "created" in stages
        assert "resolved_peer" in stages
        assert "routed" in stages
        assert "websocket_sent" in stages
        assert "pane_injected" not in stages  # the whole point of the fix
        assert "hook_received" not in stages

    async def test_injected_ack_records_pane_injected(self, env):
        # send_ask returns an injected delivery_ack -> truthful pane_injected.
        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        harness.message_router.send_ask.return_value = {"status": "injected"}
        r = await client.post("/ask", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 200, r.text
        cid = r.json()["correlation_id"]
        stages = [row.stage for row in harness.delivery_trace_store.stages_for(cid)]
        assert "hook_received" in stages
        assert "pane_injected" in stages

    async def test_ask_injection_failed_records_injection_failed_not_no_connection(self, env):
        # The hook is reached but the pane rejects injection. The trace must say
        # injection_failed (with hook_received), NOT no_connection.
        from repowire.daemon.websocket_transport import DeliveryInjectionError

        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        harness.message_router.send_ask.side_effect = DeliveryInjectionError(
            "Ask injection failed: pane busy", hook_delivery={"status": "failed"}
        )
        r = await client.post("/ask", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 503, r.text
        # The 503 body doesn't carry the cid; inspect the single ask trace directly.
        rows = harness.delivery_trace_store._conn.execute(
            "SELECT stage FROM delivery_traces WHERE kind='ask' ORDER BY seq"
        ).fetchall()
        stages = [row["stage"] for row in rows]
        assert "hook_received" in stages
        assert "injection_failed" in stages
        assert "no_connection" not in stages

    async def test_busy_notify_with_injected_records_pending_and_pane_injected(self, env):
        # A BUSY recipient queues the paste, but if the hook acked injected the
        # terminal outcome must still be traced (pending must not suppress it).
        from repowire.protocol.peers import PeerStatus

        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        peer = await harness.registry.get_peer(bob)
        peer.status = PeerStatus.BUSY
        harness.message_router.send_notification.return_value = {"status": "injected"}
        r = await client.post("/notify", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 200, r.text
        did = r.json()["delivery_id"]
        stages = [row.stage for row in harness.delivery_trace_store.stages_for(did)]
        assert "pending" in stages
        assert "pane_injected" in stages

    async def test_legacy_ws_notify_no_ack_records_transport_ws(self, env):
        # Legacy WS hook returns no delivery_ack -> websocket_sent with transport=ws,
        # NOT acp.
        client, harness = env
        await _register(client, "alice")
        bob = await _register(client, "bob")
        harness.message_router.send_notification.return_value = None
        r = await client.post("/notify", json={"to_peer": bob, "from_peer": "alice", "text": "hi"})
        assert r.status_code == 200, r.text
        did = r.json()["delivery_id"]
        rows = harness.delivery_trace_store.stages_for(did)
        ws_sent = [row for row in rows if row.stage == "websocket_sent"]
        assert ws_sent, "expected a websocket_sent stage"
        assert ws_sent[0].detail.get("transport") == "ws"

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
