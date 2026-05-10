"""Tests for AskTracker — slim ask/ack lifecycle state."""

from datetime import datetime, timedelta, timezone

import pytest

from repowire.daemon.ask_tracker import AskTracker


@pytest.fixture
def tracker():
    return AskTracker(ttl_hours=24.0)


class TestRegister:
    async def test_returns_correlation_id(self, tracker):
        cid = await tracker.register("from", "from", "to-id", "to", "hello")
        assert cid.startswith("ask-")

    async def test_custom_id(self, tracker):
        cid = await tracker.register(
            "from", "from", "to-id", "to", "hello", correlation_id="ask-abc",
        )
        assert cid == "ask-abc"

    async def test_stores_fields(self, tracker):
        cid = await tracker.register(
            "f-id", "f", "t-id", "t", "msg", reply_to="prior-cid",
        )
        ask = await tracker.get(cid)
        assert ask.from_peer_id == "f-id"
        assert ask.to_peer_name == "t"
        assert ask.text == "msg"
        assert ask.reply_to == "prior-cid"
        assert ask.closed is False

    async def test_retry_with_existing_id_returns_same(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x", correlation_id="ask-dup")
        again = await tracker.register("a", "a", "b-id", "b", "different", correlation_id="ask-dup")
        assert again == cid
        # original entry preserved
        ask = await tracker.get(cid)
        assert ask.text == "x"


class TestClose:
    async def test_closes_open_ask(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        closed = await tracker.close(cid, reason="ack")
        assert closed is not None
        ask = await tracker.get(cid)
        assert ask.closed
        assert ask.close_reason == "ack"

    async def test_idempotent(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.close(cid, reason="ack")
        again = await tracker.close(cid, reason="ack_with_msg")
        assert again is None  # already closed

    async def test_unknown(self, tracker):
        assert await tracker.close("nope", reason="ack") is None


class TestPendingForPeer:
    async def test_returns_open_asks(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        result = await tracker.pending_for_peer("b-id")
        assert len(result) == 1
        assert result[0].correlation_id == cid

    async def test_repeats_until_closed(self, tracker):
        """Open asks reappear on every poll — no once-only flag."""
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        for _ in range(3):
            result = await tracker.pending_for_peer("b-id")
            assert len(result) == 1
        # After ack, gone
        await tracker.close(cid, reason="ack")
        assert await tracker.pending_for_peer("b-id") == []

    async def test_skips_closed(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.close(cid, reason="ack")
        assert await tracker.pending_for_peer("b-id") == []

    async def test_caps_to_max(self, tracker):
        for i in range(15):
            await tracker.register("a", "a", "b-id", "b", f"x{i}")
        result = await tracker.pending_for_peer("b-id", max_results=5)
        assert len(result) == 5

    async def test_newest_first(self, tracker):
        cid_old = await tracker.register("a", "a", "b-id", "b", "old")
        cid_new = await tracker.register("a", "a", "b-id", "b", "new")
        ask_old = await tracker.get(cid_old)
        ask_old.created_at = datetime.now(timezone.utc) - timedelta(minutes=5)
        result = await tracker.pending_for_peer("b-id")
        assert result[0].correlation_id == cid_new

    async def test_other_peer_excluded(self, tracker):
        await tracker.register("a", "a", "b-id", "b", "x")
        result = await tracker.pending_for_peer("other-id")
        assert result == []


class TestForgetPeer:
    async def test_drops_asks_to_or_from_peer(self, tracker):
        cid_to = await tracker.register("a", "a", "b-id", "b", "x")
        cid_from = await tracker.register("b", "b", "c-id", "c", "y")
        # Forget "b-id": cid_to (to_peer_id=b-id) drops; cid_from is unaffected
        # (from_peer_id="b" not "b-id", to_peer_id="c-id").
        dropped = await tracker.forget_peer("b-id")
        assert dropped == 1
        assert await tracker.get(cid_to) is None
        assert await tracker.get(cid_from) is not None


class TestEvictExpired:
    async def test_evicts_old(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        ask = await tracker.get(cid)
        ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)
        evicted = await tracker.evict_expired()
        assert evicted == 1
        assert await tracker.get(cid) is None

    async def test_keeps_fresh(self, tracker):
        await tracker.register("a", "a", "b-id", "b", "x")
        evicted = await tracker.evict_expired()
        assert evicted == 0
