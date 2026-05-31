"""Tests for /ask, /ack, and /asks/* HTTP routes."""

from unittest.mock import AsyncMock

import pytest

from repowire.daemon.deps import cleanup_deps
from repowire.daemon.routes import asks, peers
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.peers import PeerStatus

from .conftest import async_client_for, make_daemon_app

ROUTERS = (peers.router, asks.router)


def _make_app(tmp_path):
    harness = make_daemon_app(tmp_path, ROUTERS)
    # Stub the wire-level send so /ask and /ack don't fail on missing transport.
    harness.message_router.send_notification = AsyncMock()
    harness.message_router.send_ask = AsyncMock()
    return (
        harness.app,
        harness.registry,
        harness.ask_tracker,
        harness.message_router,
    )


@pytest.fixture
async def env(tmp_path):
    app, registry, at, msg_router = _make_app(tmp_path)
    async with async_client_for(app) as c:
        yield c, registry, at, msg_router
    cleanup_deps()


async def _register_peer(client, name: str, pane_id: str | None = None) -> str:
    body: dict = {
        "name": name,
        "path": f"/tmp/{name}",
        "circle": "default",
        "backend": "claude-code",
    }
    if pane_id:
        body["pane_id"] = pane_id
    r = await client.post("/peers", json=body)
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


async def _register_peer_info(client, name: str, pane_id: str | None = None) -> dict:
    display_name = await _register_peer(client, name, pane_id=pane_id)
    r = await client.get(f"/peers/{display_name}")
    assert r.status_code == 200, r.text
    return r.json()


class TestAsk:
    async def test_returns_correlation_id(self, env):
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice",
            "to_peer": bob,
            "text": "what's up?",
        })
        assert r.status_code == 200
        body = r.json()
        assert body["correlation_id"].startswith("ask-")

    async def test_unknown_peer_returns_404(self, env):
        client, _, _, _ = env
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": "ghost", "text": "?",
        })
        assert r.status_code == 404

    async def test_transport_error_returns_503_and_rolls_back(self, env):
        client, registry, at, msg_router = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        msg_router.send_ask.side_effect = TransportError("No connection")

        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        assert r.status_code == 503
        # No phantom open ask should remain
        assert at.open_count() == 0
        peer = await registry.get_peer(bob)
        assert peer is not None
        assert peer.status == PeerStatus.OFFLINE

    async def test_cli_polling_peer_keeps_ask_when_transport_absent(self, env):
        client, registry, at, msg_router = env
        await _register_peer(client, "alice")
        r = await client.post("/peers", json={
            "name": "agy",
            "path": "/tmp/agy",
            "circle": "default",
            "backend": "antigravity",
            "metadata": {"repowire_cli_fallback": True},
        })
        assert r.status_code == 200, r.text
        agy = r.json()["display_name"]
        msg_router.send_ask.side_effect = TransportError("No connection")

        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": agy, "text": "?",
        })

        assert r.status_code == 200, r.text
        cid = r.json()["correlation_id"]
        ask = await at.get(cid)
        assert ask is not None
        assert not ask.closed
        peer = await registry.get_peer(agy)
        assert peer is not None
        assert peer.status == PeerStatus.ONLINE
        pending = await client.get("/asks/pending", params={"peer_id": ask.to_peer_id})
        assert pending.status_code == 200
        assert pending.json()["asks"][0]["correlation_id"] == cid

    async def test_reply_to_closes_prior(self, env):
        client, _, at, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "first",
        })
        first_cid = r.json()["correlation_id"]

        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "follow-up",
            "reply_to": first_cid,
        })
        assert r.status_code == 200
        prior = await at.get(first_cid)
        assert prior.closed
        assert prior.close_reason == "reply_to"

    async def test_reply_to_not_closed_when_send_fails(self, env):
        client, _, at, msg_router = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        # First ask succeeds
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "first",
        })
        first_cid = r.json()["correlation_id"]
        prior = await at.get(first_cid)
        assert not prior.closed

        # Second ask with reply_to fails to send → prior must remain open
        msg_router.send_ask.side_effect = TransportError("No connection")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "follow-up",
            "reply_to": first_cid,
        })
        assert r.status_code == 503
        prior = await at.get(first_cid)
        assert not prior.closed

    async def test_peer_id_sender_is_canonicalized_for_reply_path(self, env):
        client, registry, at, msg_router = env
        alice = await _register_peer_info(client, "alice")
        bob = await _register_peer_info(client, "bob")

        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"], "to_peer": bob["peer_id"], "text": "?",
        })

        assert r.status_code == 200
        cid = r.json()["correlation_id"]
        ask = await at.get(cid)
        assert ask.from_peer_id == alice["peer_id"]
        assert ask.from_peer_name == alice["display_name"]
        assert ask.to_peer_id == bob["peer_id"]
        assert ask.to_peer_name == bob["display_name"]
        assert msg_router.send_ask.await_args.kwargs["from_peer"] == alice["display_name"]
        assert msg_router.send_ask.await_args.kwargs["to_session_id"] == bob["peer_id"]
        event = registry.get_events()[-1]
        assert event["type"] == "ask"
        assert event["from"] == alice["display_name"]
        assert event["to"] == bob["display_name"]
        assert event["from_peer_id"] == alice["peer_id"]
        assert event["to_peer_id"] == bob["peer_id"]
        assert event["self_target"] is False

    async def test_ask_carries_attachments_to_event_and_wire(self, env):
        client, registry, _, msg_router = env
        alice = await _register_peer_info(client, "alice")
        bob = await _register_peer_info(client, "bob")

        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"],
            "to_peer": bob["peer_id"],
            "text": "see image",
            "attachments": [{
                "id": "att123",
                "path": "/tmp/att123.png",
                "filename": "diagram.png",
                "size": 123,
                "content_type": "image/png",
            }],
        })

        assert r.status_code == 200
        kwargs = msg_router.send_ask.await_args.kwargs
        assert kwargs["attachments"][0].id == "att123"
        event = registry.get_events()[-1]
        assert event["attachments"][0]["filename"] == "diagram.png"

    async def test_self_ask_succeeds_with_diagnostic_event_marker(self, env):
        client, registry, at, msg_router = env
        alice = await _register_peer_info(client, "alice")

        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"], "to_peer": alice["peer_id"], "text": "loopback?",
        })

        assert r.status_code == 200
        assert at.open_count() == 1
        msg_router.send_ask.assert_awaited_once()
        event = registry.get_events()[-1]
        assert event["type"] == "ask"
        assert event["from_peer_id"] == alice["peer_id"]
        assert event["to_peer_id"] == alice["peer_id"]
        assert event["self_target"] is True


class TestAck:
    async def test_bare_ack_closes(self, env):
        client, _, at, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack"

    async def test_ack_with_msg_delivers_reply(self, env):
        client, _, at, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "status?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob, "message": "all good",
        })
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack_with_msg"

    async def test_ack_reply_carries_attachments(self, env):
        client, registry, at, msg_router = env
        alice = await _register_peer_info(client, "alice")
        bob = await _register_peer_info(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"], "to_peer": bob["peer_id"], "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid,
            "from_peer": bob["display_name"],
            "message": "attached",
            "attachments": [{
                "id": "att123",
                "path": "/tmp/att123.png",
                "filename": "diagram.png",
            }],
        })

        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        kwargs = msg_router.send_notification.await_args.kwargs
        assert kwargs["attachments"][0].id == "att123"
        event = registry.get_events()[-1]
        assert event["attachments"][0]["filename"] == "diagram.png"

    async def test_ack_reply_uses_stored_peer_ids_not_reported_from_peer(self, env):
        client, registry, at, msg_router = env
        alice = await _register_peer_info(client, "alice")
        bob = await _register_peer_info(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"], "to_peer": bob["peer_id"], "text": "?",
        })
        cid = r.json()["correlation_id"]

        # Simulate a recipient MCP process reporting the wrong local identity.
        r = await client.post("/ack", json={
            "correlation_id": cid,
            "from_peer": alice["display_name"],
            "message": "reply from bob",
        })

        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack_with_msg"
        kwargs = msg_router.send_notification.await_args.kwargs
        assert kwargs["from_peer"] == bob["display_name"]
        assert kwargs["to_session_id"] == alice["peer_id"]
        assert f"from @{bob['display_name']}" in kwargs["text"]
        event = registry.get_events()[-1]
        assert event["type"] == "notification"
        assert event["from"] == bob["display_name"]
        assert event["to"] == alice["display_name"]
        assert event["from_peer_id"] == bob["peer_id"]
        assert event["to_peer_id"] == alice["peer_id"]

    async def test_ack_reply_does_not_require_from_peer(self, env):
        client, _, at, msg_router = env
        alice = await _register_peer_info(client, "alice")
        bob = await _register_peer_info(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice["peer_id"], "to_peer": bob["peer_id"], "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.post("/ack", json={
            "correlation_id": cid,
            "message": "reply from bob",
        })

        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack_with_msg"
        kwargs = msg_router.send_notification.await_args.kwargs
        assert kwargs["from_peer"] == bob["display_name"]
        assert kwargs["to_session_id"] == alice["peer_id"]

    async def test_ack_unknown_id_404(self, env):
        client, _, _, _ = env
        r = await client.post("/ack", json={
            "correlation_id": "ask-never", "from_peer": "x",
        })
        assert r.status_code == 404

    async def test_ack_with_msg_503_when_reply_undeliverable(self, env):
        """If reply delivery fails (no live WS), ack returns 503 and ask stays open."""
        client, _, at, msg_router = env
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice, "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        # Asker's notify path now fails
        msg_router.send_notification.side_effect = TransportError("No connection")

        r = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob, "message": "all good",
        })
        assert r.status_code == 503
        ask = await at.get(cid)
        assert not ask.closed  # MUST remain open so recipient can retry

    async def test_bare_ack_succeeds_even_if_router_would_fail(self, env):
        """Bare ack doesn't deliver anything, so router state is irrelevant."""
        client, _, at, msg_router = env
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": alice, "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        msg_router.send_notification.side_effect = TransportError("dead")

        r = await client.post("/ack", json={"correlation_id": cid, "from_peer": bob})
        assert r.status_code == 200
        ask = await at.get(cid)
        assert ask.closed

    async def test_ack_idempotent(self, env):
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        r2 = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })
        assert r2.status_code == 200

    async def test_ack_with_msg_on_closed_ask_returns_410(self, env):
        client, _, _, msg_router = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]
        await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob,
        })

        r2 = await client.post("/ack", json={
            "correlation_id": cid, "from_peer": bob, "message": "late reply",
        })

        assert r2.status_code == 410
        assert "already closed" in r2.json()["detail"]
        assert msg_router.send_notification.await_count == 0


class TestPendingAsks:
    async def test_returns_open_asks(self, env):
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%50")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        r = await client.get("/asks/pending?pane_id=%2550")
        assert r.status_code == 200
        body = r.json()
        assert len(body["asks"]) == 1
        assert body["asks"][0]["correlation_id"] == cid
        # Slim shape — no current_turn_seq, no picked_up_at
        assert "current_turn_seq" not in body
        assert "picked_up_at" not in body["asks"][0]

    async def test_repeats_on_each_poll(self, env):
        """Open asks reappear every poll until acked."""
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob", pane_id="%50")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        for _ in range(3):
            r = await client.get("/asks/pending?pane_id=%2550")
            assert len(r.json()["asks"]) == 1

        # After ack, gone
        await client.post("/ack", json={"correlation_id": cid, "from_peer": bob})
        r = await client.get("/asks/pending?pane_id=%2550")
        assert r.json()["asks"] == []

    async def test_unknown_pane(self, env):
        client, _, _, _ = env
        r = await client.get("/asks/pending?pane_id=%25nope")
        assert r.status_code == 404

    async def test_lookup_by_peer_id(self, env):
        """Pi transport polls by peer_id (multiple sessions share a pane)."""
        client, _, _, _ = env
        await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask", json={
            "from_peer": "alice", "to_peer": bob, "text": "ping",
        })
        cid = r.json()["correlation_id"]

        r = await client.get(f"/asks/pending?peer_id={bob}")
        assert r.status_code == 200
        body = r.json()
        assert len(body["asks"]) == 1
        assert body["asks"][0]["correlation_id"] == cid

    async def test_requires_pane_or_peer(self, env):
        client, _, _, _ = env
        r = await client.get("/asks/pending")
        assert r.status_code == 400

    async def test_rejects_both_pane_and_peer(self, env):
        client, _, _, _ = env
        r = await client.get("/asks/pending?pane_id=%2550&peer_id=foo")
        assert r.status_code == 400

    async def test_unknown_peer_id(self, env):
        client, _, _, _ = env
        r = await client.get("/asks/pending?peer_id=does-not-exist")
        assert r.status_code == 404

    async def test_direction_outbound_returns_asks_this_peer_opened(self, env):
        """direction=outbound returns asks where this peer is the sender."""
        client, _, _, _ = env
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        # Use the actual assigned display_name so from_peer_id resolves
        # to alice's peer_id (and not the literal "alice" string fallback).
        r = await client.post("/ask", json={
            "from_peer": alice, "to_peer": bob, "text": "?",
        })
        cid = r.json()["correlation_id"]

        # Default direction (inbound): alice has no inbound asks
        r = await client.get(f"/asks/pending?peer_id={alice}")
        assert r.status_code == 200
        assert r.json()["asks"] == []

        # direction=outbound: alice has one outbound ask
        r = await client.get(f"/asks/pending?peer_id={alice}&direction=outbound")
        assert r.status_code == 200
        body = r.json()
        assert len(body["asks"]) == 1
        assert body["asks"][0]["correlation_id"] == cid
        assert body["asks"][0]["direction"] == "outbound"
        assert body["asks"][0]["to_peer"] == bob

        # direction=both: union (alice still has only the outbound)
        r = await client.get(f"/asks/pending?peer_id={alice}&direction=both")
        assert len(r.json()["asks"]) == 1

        # Bob's inbound view is unchanged by default
        r = await client.get(f"/asks/pending?peer_id={bob}")
        assert len(r.json()["asks"]) == 1
        assert r.json()["asks"][0]["direction"] == "inbound"

    async def test_direction_invalid_returns_400(self, env):
        client, _, _, _ = env
        bob = await _register_peer(client, "bob")
        r = await client.get(f"/asks/pending?peer_id={bob}&direction=sideways")
        assert r.status_code == 400


class TestDeprecatedNoOps:
    """Compat: legacy transports may still POST these. Should return 200 silently."""

    async def test_picked_up_returns_200(self, env):
        client, _, _, _ = env
        r = await client.post("/asks/legacy-cid/picked_up", json={"correlation_id": "legacy-cid"})
        assert r.status_code == 200

    async def test_mark_reminded_returns_200(self, env):
        client, _, _, _ = env
        r = await client.post(
            "/asks/legacy-cid/mark_reminded",
            json={"correlation_id": "legacy-cid"},
        )
        assert r.status_code == 200


class TestAskMany:
    async def test_fanout_aggregates_replies_and_bare_acks(self, env):
        # End-to-end: POST /ask-many to two peers, ack one with a message and
        # one bare, then GET the aggregated result (acceptance: all-reply path +
        # child-ack routing under a parent).
        client, _, _, _ = env
        asker = await _register_peer(client, "asker")
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")

        r = await client.post("/ask-many", json={
            "from_peer": asker,
            "to_peers": [alice, bob],
            "text": "standup?",
        })
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["parent_id"].startswith("askm-")
        children = {c["peer"]: c for c in body["children"]}
        assert set(children) == {alice, bob}
        assert all(c["correlation_id"].startswith("ask-") for c in children.values())

        # alice replies with a message, bob bare-acks
        await client.post("/ack", json={
            "correlation_id": children[alice]["correlation_id"],
            "message": "done",
        })
        await client.post("/ack", json={
            "correlation_id": children[bob]["correlation_id"],
        })

        res = await client.get(f"/ask-many/{body['parent_id']}")
        assert res.status_code == 200, res.text
        agg = res.json()
        assert agg["state"] == "complete"
        assert agg["rollup"] == {"total": 2, "acked": 1, "replied": 1, "pending": 0, "failed": 0}
        by_peer = {c["peer"]: c for c in agg["children"]}
        assert by_peer[alice]["status"] == "replied"
        assert by_peer[alice]["reply"] == "done"
        assert by_peer[bob]["status"] == "acked"

    async def test_partial_when_one_peer_pending(self, env):
        client, _, _, _ = env
        asker = await _register_peer(client, "asker")
        alice = await _register_peer(client, "alice")
        bob = await _register_peer(client, "bob")
        r = await client.post("/ask-many", json={
            "from_peer": asker, "to_peers": [alice, bob], "text": "?",
        })
        children = {c["peer"]: c for c in r.json()["children"]}
        await client.post("/ack", json={"correlation_id": children[alice]["correlation_id"]})

        agg = (await client.get(f"/ask-many/{r.json()['parent_id']}")).json()
        # bob still open, deadline not passed -> pending state, 1 pending child
        assert agg["state"] == "pending"
        assert agg["rollup"]["pending"] == 1
        assert agg["rollup"]["acked"] == 1

    async def test_unknown_peer_is_failed_child_not_fatal(self, env):
        client, _, _, _ = env
        asker = await _register_peer(client, "asker")
        alice = await _register_peer(client, "alice")
        r = await client.post("/ask-many", json={
            "from_peer": asker, "to_peers": [alice, "ghost"], "text": "?",
        })
        assert r.status_code == 200, r.text
        agg = (await client.get(f"/ask-many/{r.json()['parent_id']}")).json()
        by_peer = {c["peer"]: c for c in agg["children"]}
        assert by_peer["ghost"]["status"] == "failed"
        assert by_peer[alice]["status"] in {"pending", "acked"}

    async def test_empty_peer_list_422(self, env):
        client, _, _, _ = env
        asker = await _register_peer(client, "asker")
        r = await client.post("/ask-many", json={
            "from_peer": asker, "to_peers": [], "text": "?",
        })
        assert r.status_code == 422

    async def test_unknown_parent_404(self, env):
        client, _, _, _ = env
        r = await client.get("/ask-many/askm-nope")
        assert r.status_code == 404
