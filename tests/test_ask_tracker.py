"""Tests for AskTracker — ask/ack lifecycle state."""

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
        assert ask.picked_up is False
        assert ask.closed is False


class TestPickup:
    async def test_first_call_records(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        ok = await tracker.mark_picked_up(cid, turn_seq=1)
        assert ok
        ask = await tracker.get(cid)
        assert ask.picked_up
        assert ask.picked_up_turn_seq == 1

    async def test_idempotent_second_call(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.mark_picked_up(cid, turn_seq=1)
        ok = await tracker.mark_picked_up(cid, turn_seq=5)
        assert not ok
        ask = await tracker.get(cid)
        # Second call must NOT shift the seq — that would invalidate grace
        assert ask.picked_up_turn_seq == 1

    async def test_unknown_id(self, tracker):
        assert not await tracker.mark_picked_up("never", turn_seq=1)


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
    async def test_filters_unpicked(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        # Not yet picked up
        result = await tracker.pending_for_peer("b-id", current_turn_seq=5)
        assert result == []
        await tracker.mark_picked_up(cid, turn_seq=1)
        result = await tracker.pending_for_peer("b-id", current_turn_seq=5)
        assert len(result) == 1

    async def test_grace_window_one_turn(self, tracker):
        """picked_up_turn_seq < current_turn_seq is the rule."""
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.mark_picked_up(cid, turn_seq=3)
        # Same-turn check: 3 < 3 is false, not yet flagged
        assert await tracker.pending_for_peer("b-id", current_turn_seq=3) == []
        # Next turn: 3 < 4, flagged
        result = await tracker.pending_for_peer("b-id", current_turn_seq=4)
        assert len(result) == 1

    async def test_skips_reminded(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.mark_picked_up(cid, turn_seq=1)
        await tracker.mark_reminded(cid)
        assert await tracker.pending_for_peer("b-id", current_turn_seq=5) == []

    async def test_skips_closed(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.mark_picked_up(cid, turn_seq=1)
        await tracker.close(cid, reason="ack")
        assert await tracker.pending_for_peer("b-id", current_turn_seq=5) == []

    async def test_caps_to_max(self, tracker):
        cids = []
        for i in range(5):
            cid = await tracker.register("a", "a", "b-id", "b", f"x{i}")
            await tracker.mark_picked_up(cid, turn_seq=1)
            cids.append(cid)
        result = await tracker.pending_for_peer("b-id", current_turn_seq=5, max_results=3)
        assert len(result) == 3

    async def test_newest_first(self, tracker):
        cid_old = await tracker.register("a", "a", "b-id", "b", "old")
        cid_new = await tracker.register("a", "a", "b-id", "b", "new")
        # Manually backdate the older one to ensure ordering
        ask_old = await tracker.get(cid_old)
        ask_old.created_at = datetime.now(timezone.utc) - timedelta(minutes=5)
        await tracker.mark_picked_up(cid_old, turn_seq=1)
        await tracker.mark_picked_up(cid_new, turn_seq=1)
        result = await tracker.pending_for_peer("b-id", current_turn_seq=5)
        assert result[0].correlation_id == cid_new

    async def test_other_peer_excluded(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        await tracker.mark_picked_up(cid, turn_seq=1)
        result = await tracker.pending_for_peer("other-id", current_turn_seq=5)
        assert result == []


class TestTurnCounter:
    async def test_increments_per_peer(self, tracker):
        assert await tracker.increment_turn("p1") == 1
        assert await tracker.increment_turn("p1") == 2
        assert await tracker.increment_turn("p2") == 1


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
