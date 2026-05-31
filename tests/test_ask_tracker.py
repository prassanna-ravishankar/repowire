"""Tests for AskTracker — slim ask/ack lifecycle state."""

from datetime import datetime, timedelta, timezone

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.protocol.questions import Answer, Question


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


# ----------------------------------------------------------------------
# #207 — identity-tuple rebind helpers
# ----------------------------------------------------------------------

from repowire.daemon.ask_tracker import AskerIdentity  # noqa: E402


def _ident(
    *,
    display_name="asker",
    circle="default",
    backend="claude-code",
    path="/repo/asker",
    machine="localhost",
) -> AskerIdentity:
    return AskerIdentity(
        display_name=display_name,
        circle=circle,
        backend=backend,
        path=path,
        machine=machine,
    )


class TestSetPendingReplyIdentity:
    async def test_records_identity_when_passed(self, tracker):
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        ok = await tracker.set_pending_reply(cid, "framed", identity=_ident())
        assert ok is True
        ask = await tracker.get(cid)
        assert ask is not None
        assert ask.asker_identity == _ident()
        assert ask.pending_reply == "framed"
        assert ask.pending_reply_at is not None

    async def test_accepts_none_identity(self, tracker):
        """Stash without identity is supported (pass-1 reconnect still works)."""
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        ok = await tracker.set_pending_reply(cid, "framed", identity=None)
        assert ok is True
        ask = await tracker.get(cid)
        assert ask is not None and ask.asker_identity is None

    async def test_default_identity_is_none(self, tracker):
        """Back-compat: set_pending_reply without identity kwarg still works."""
        cid = await tracker.register("a", "a", "b-id", "b", "x")
        ok = await tracker.set_pending_reply(cid, "framed")
        assert ok is True
        ask = await tracker.get(cid)
        assert ask is not None and ask.asker_identity is None

    async def test_answered_question_can_carry_pending_reply_when_explicit(self, tracker):
        cid = await tracker.register(
            "asker-id", "asker", "b-id", "b", "x",
            question=Question(kind="text"),
        )
        await tracker.answer(cid, Answer(text="done"))

        assert await tracker.set_pending_reply(cid, "framed") is False
        ok = await tracker.set_pending_reply(
            cid, "framed", allow_answered_question=True,
        )

        assert ok is True
        ask = await tracker.get(cid)
        assert ask is not None
        assert ask.closed is True
        assert ask.close_reason == "answered"
        assert ask.pending_reply == "framed"

    async def test_closed_non_answered_ask_cannot_carry_pending_reply(self, tracker):
        cid = await tracker.register("asker-id", "asker", "b-id", "b", "x")
        await tracker.close(cid, reason="ack")

        ok = await tracker.set_pending_reply(
            cid, "framed", allow_answered_question=True,
        )

        assert ok is False
        ask = await tracker.get(cid)
        assert ask is not None and ask.pending_reply is None

    async def test_take_pending_replies_includes_answered_question_stashes(self, tracker):
        cid = await tracker.register(
            "asker-id", "asker", "b-id", "b", "x",
            question=Question(kind="text"),
        )
        await tracker.answer(cid, Answer(text="done"))
        await tracker.set_pending_reply(
            cid, "framed", allow_answered_question=True,
        )

        out = await tracker.take_pending_replies_for_asker("asker-id")

        assert len(out) == 1 and out[0].correlation_id == cid

    async def test_mark_pending_reply_delivered_keeps_answered_question_closed(self, tracker):
        cid = await tracker.register(
            "asker-id", "asker", "b-id", "b", "x",
            question=Question(kind="text"),
        )
        await tracker.answer(cid, Answer(text="done"))
        await tracker.set_pending_reply(
            cid, "framed", allow_answered_question=True,
        )

        ok = await tracker.mark_pending_reply_delivered(cid)

        assert ok is True
        ask = await tracker.get(cid)
        assert ask is not None
        assert ask.closed is True
        assert ask.close_reason == "answered"
        assert ask.pending_reply is None


class TestTakeOrphanPendingRepliesMatching:
    async def test_requires_all_fields(self, tracker):
        cid = await tracker.register("asker-old-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed", identity=_ident())
        # Wrong machine → no match (every field must match exactly)
        out = await tracker.take_orphan_pending_replies_matching(
            display_name="asker", circle="default", backend="claude-code",
            path="/repo/asker", machine="other-host",
            live_peer_ids=set(),
        )
        assert out == []
        # Right tuple AND original peer_id not in live set → match
        out = await tracker.take_orphan_pending_replies_matching(
            display_name="asker", circle="default", backend="claude-code",
            path="/repo/asker", machine="localhost",
            live_peer_ids=set(),
        )
        assert len(out) == 1 and out[0].correlation_id == cid

    async def test_skipped_when_original_peer_still_live(self, tracker):
        cid = await tracker.register("asker-old-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed", identity=_ident())
        out = await tracker.take_orphan_pending_replies_matching(
            display_name="asker", circle="default", backend="claude-code",
            path="/repo/asker", machine="localhost",
            live_peer_ids={"asker-old-id"},  # original still alive ⇒ no orphan
        )
        assert out == []

    async def test_no_identity_means_no_match(self, tracker):
        cid = await tracker.register("asker-old-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed")  # no identity captured
        out = await tracker.take_orphan_pending_replies_matching(
            display_name="asker", circle="default", backend="claude-code",
            path="/repo/asker", machine="localhost",
            live_peer_ids=set(),
        )
        assert out == []


class TestSnapshotHelpers:
    async def test_snapshot_pending_replies_for_peer_does_not_mutate(self, tracker):
        cid = await tracker.register("asker-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed", identity=_ident())
        snap = await tracker.snapshot_pending_replies_for_peer("asker-id")
        assert len(snap) == 1 and snap[0].correlation_id == cid
        # Tracker still has the ask after snapshot
        assert await tracker.get(cid) is not None

    async def test_snapshot_pending_replies_for_peer_excludes_unstashed(self, tracker):
        await tracker.register("asker-id", "asker", "b-id", "b", "x")  # no stash
        snap = await tracker.snapshot_pending_replies_for_peer("asker-id")
        assert snap == []

    async def test_snapshot_expired_pending_replies_filters_by_ttl(self, tracker):
        cid = await tracker.register("asker-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed", identity=_ident())
        # Not expired yet
        snap = await tracker.snapshot_expired_pending_replies()
        assert snap == []
        # Backdate to past TTL
        ask = await tracker.get(cid)
        ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)
        snap = await tracker.snapshot_expired_pending_replies()
        assert len(snap) == 1 and snap[0].correlation_id == cid
        # Snapshot is non-mutating
        assert await tracker.get(cid) is not None

    async def test_snapshot_expired_excludes_unstashed(self, tracker):
        cid = await tracker.register("asker-id", "asker", "b-id", "b", "x")
        ask = await tracker.get(cid)
        ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)
        snap = await tracker.snapshot_expired_pending_replies()
        assert snap == []


class TestEvictExpiredStashedFlag:
    async def test_maybe_evict_skips_stashed(self, tracker):
        cid = await tracker.register("asker-id", "asker", "b-id", "b", "x")
        await tracker.set_pending_reply(cid, "framed", identity=_ident())
        ask = await tracker.get(cid)
        ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)
        # Default include_stashed=True drops it
        # but include_stashed=False (the path used by _maybe_evict_expired)
        # leaves it.
        evicted = await tracker.evict_expired(include_stashed=False)
        assert evicted == 0
        assert await tracker.get(cid) is not None
        evicted = await tracker.evict_expired(include_stashed=True)
        assert evicted == 1
        assert await tracker.get(cid) is None
