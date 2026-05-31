"""ask-many fanout aggregation (repowire-sip.8)."""

from __future__ import annotations

from datetime import timedelta

import pytest

from repowire.daemon.ask_many import AskManyChild, AskManyTracker
from repowire.daemon.ask_tracker import AskTracker


async def _register_child(at: AskTracker, parent_id: str, to_name: str) -> str:
    return await at.register(
        from_peer_id="asker",
        from_peer_name="asker",
        to_peer_id=f"id-{to_name}",
        to_peer_name=to_name,
        text="q",
        parent_id=parent_id,
    )


@pytest.mark.asyncio
async def test_all_reply_rolls_up_complete_with_bodies():
    at = AskTracker()
    am = AskManyTracker(at)
    parent = await am.create(from_peer_id="asker", from_peer_name="asker", text="q")

    for name in ("alice", "bob"):
        cid = await _register_child(at, parent.parent_id, name)
        await am.add_child(parent.parent_id, AskManyChild(peer_name=name, correlation_id=cid))
        # simulate ack-with-message: capture reply, then close
        await at.capture_child_reply(cid, f"{name} says hi")
        await at.close(cid, reason="ack_with_msg")

    status = await am.status(parent.parent_id)
    assert status["state"] == "complete"
    assert status["timed_out"] is False
    assert status["rollup"] == {"total": 2, "acked": 0, "replied": 2, "pending": 0, "failed": 0}
    replies = {c["peer"]: c["reply"] for c in status["children"]}
    assert replies == {"alice": "alice says hi", "bob": "bob says hi"}


@pytest.mark.asyncio
async def test_bare_ack_is_acked_not_replied():
    at = AskTracker()
    am = AskManyTracker(at)
    parent = await am.create(from_peer_id="asker", from_peer_name="asker", text="q")
    cid = await _register_child(at, parent.parent_id, "alice")
    await am.add_child(parent.parent_id, AskManyChild(peer_name="alice", correlation_id=cid))
    await at.close(cid, reason="ack")  # bare ack, no body

    status = await am.status(parent.parent_id)
    assert status["rollup"]["acked"] == 1
    assert status["rollup"]["replied"] == 0
    assert status["children"][0]["status"] == "acked"


@pytest.mark.asyncio
async def test_partial_timeout_when_deadline_passed_with_open_child():
    at = AskTracker()
    am = AskManyTracker(at)
    parent = await am.create(
        from_peer_id="asker", from_peer_name="asker", text="q", timeout_seconds=60,
    )
    done = await _register_child(at, parent.parent_id, "alice")
    await am.add_child(parent.parent_id, AskManyChild(peer_name="alice", correlation_id=done))
    await at.close(done, reason="ack")

    pending = await _register_child(at, parent.parent_id, "bob")
    await am.add_child(parent.parent_id, AskManyChild(peer_name="bob", correlation_id=pending))

    # before deadline -> pending; after -> partial
    before = await am.status(parent.parent_id, now=parent.created_at + timedelta(seconds=10))
    assert before["state"] == "pending"
    assert before["timed_out"] is False

    after = await am.status(parent.parent_id, now=parent.created_at + timedelta(seconds=120))
    assert after["state"] == "partial"
    assert after["timed_out"] is True
    assert after["rollup"] == {"total": 2, "acked": 1, "replied": 0, "pending": 1, "failed": 0}


@pytest.mark.asyncio
async def test_failed_child_counts_as_failed():
    at = AskTracker()
    am = AskManyTracker(at)
    parent = await am.create(from_peer_id="asker", from_peer_name="asker", text="q")
    # a child that never registered (resolution failure) — no correlation_id
    await am.add_child(
        parent.parent_id,
        AskManyChild(peer_name="ghost", delivery_error="peer not found: ghost"),
    )
    # a child closed send_failed (delivery failure after register)
    cid = await _register_child(at, parent.parent_id, "bob")
    await am.add_child(parent.parent_id, AskManyChild(peer_name="bob", correlation_id=cid))
    await at.close(cid, reason="send_failed")

    status = await am.status(parent.parent_id)
    assert status["rollup"]["failed"] == 2
    statuses = {c["peer"]: c["status"] for c in status["children"]}
    assert statuses == {"ghost": "failed", "bob": "failed"}


@pytest.mark.asyncio
async def test_unknown_parent_returns_none():
    am = AskManyTracker(AskTracker())
    assert await am.status("askm-nope") is None


@pytest.mark.asyncio
async def test_capture_child_reply_is_noop_for_non_child_ask():
    at = AskTracker()
    cid = await at.register(
        from_peer_id="a", from_peer_name="a", to_peer_id="b", to_peer_name="b", text="q",
    )
    await at.capture_child_reply(cid, "ignored")  # no parent_id -> no-op
    ask = await at.get(cid)
    assert ask is not None and ask.reply_text is None


@pytest.mark.asyncio
async def test_acp_child_reply_not_captured_when_asker_delivery_fails():
    # codex catch: an ACP child whose reply delivery to the asker fails must NOT
    # expose a reply body — capture only after delivery succeeds, matching the
    # WS /ack 503 path. Here notify raises TransportError -> child stays pending.
    from unittest.mock import AsyncMock

    from repowire.daemon.routes.asks import _acp_complete
    from repowire.daemon.websocket_transport import TransportError

    at = AskTracker()
    am = AskManyTracker(at)
    parent = await am.create(from_peer_id="asker", from_peer_name="asker", text="q")
    cid = await _register_child(at, parent.parent_id, "alice")
    await am.add_child(parent.parent_id, AskManyChild(peer_name="alice", correlation_id=cid))

    registry = AsyncMock()
    registry.notify = AsyncMock(side_effect=TransportError("asker offline"))
    registry.add_event = lambda *a, **k: None
    registry.set_pending_reply = AsyncMock()

    await _acp_complete(
        correlation_id=cid, reply="the answer", error=None,
        ask_tracker=at, peer_registry=registry,
    )

    ask = await at.get(cid)
    assert ask is not None and ask.reply_text is None  # not captured
    status = await am.status(parent.parent_id)
    child = status["children"][0]
    assert child["status"] == "pending"  # stayed open (stashed for redelivery)
    assert child["reply"] is None
