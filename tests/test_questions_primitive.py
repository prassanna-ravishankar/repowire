"""Mesh questions primitive — protocol models + AskTracker answer machinery.

Layer A of the Mesh Questions epic (repowire-iz6): the shared Question/Answer
shape and the AskTracker register(question=)/wait_for_answer/answer seam that
blocking transports (ACP, PreToolUse) await and any answer-path resolves.
"""

from __future__ import annotations

import asyncio

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.protocol.questions import Answer, Question, QuestionOption


def _allow_deny() -> Question:
    return Question(
        kind="choice",
        options=[
            QuestionOption(id="allow", title="Allow"),
            QuestionOption(id="deny", title="Deny"),
        ],
        blocking=True,
        timeout_seconds=0.05,
        default_answer=Answer(option_id="deny", outcome="timed_out"),
        scope="tool_permission",
    )


async def _register_question(at: AskTracker, q: Question) -> str:
    return await at.register(
        from_peer_id="agent", from_peer_name="agent",
        to_peer_id="human", to_peer_name="human",
        text="run rm -rf?", question=q,
    )


# --- protocol model validation ---

def test_choice_requires_known_option():
    q = _allow_deny()
    assert q.validate_answer(Answer(option_id="allow")) is None
    assert q.validate_answer(Answer(option_id="nope")) == "unknown option_id: nope"
    assert "requires an option_id" in (q.validate_answer(Answer(text="hmm")) or "")


def test_timeout_and_cancel_bypass_option_validation():
    q = _allow_deny()
    assert q.validate_answer(Answer(outcome="timed_out")) is None
    assert q.validate_answer(Answer(outcome="cancelled")) is None


def test_text_and_acknowledge_are_permissive():
    assert Question(kind="text").validate_answer(Answer(text="anything")) is None
    assert Question(kind="acknowledge").validate_answer(Answer(outcome="acknowledged")) is None


# --- AskTracker answer machinery ---

@pytest.mark.asyncio
async def test_answer_records_truth_and_closes():
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())
    ask = await at.answer(cid, Answer(option_id="allow"))
    assert ask.answer is not None and ask.answer.option_id == "allow"
    assert ask.closed and ask.close_reason == "answered"


@pytest.mark.asyncio
async def test_answer_validates_option_id_in_daemon():
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())
    with pytest.raises(ValueError, match="unknown option_id"):
        await at.answer(cid, Answer(option_id="bogus"))
    # ask stays open (no answer recorded)
    ask = await at.get(cid)
    assert ask is not None and ask.answer is None and not ask.closed


@pytest.mark.asyncio
async def test_first_answer_wins():
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())
    await at.answer(cid, Answer(option_id="allow"))
    with pytest.raises(AskTracker.AlreadyAnsweredError):
        await at.answer(cid, Answer(option_id="deny"))
    ask = await at.get(cid)
    assert ask is not None and ask.answer.option_id == "allow"  # unchanged


@pytest.mark.asyncio
async def test_wait_for_answer_resolves_when_answered():
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())

    async def answer_soon():
        await asyncio.sleep(0.01)
        await at.answer(cid, Answer(option_id="allow"))

    asyncio.create_task(answer_soon())
    result = await at.wait_for_answer(cid, timeout_seconds=1.0, default_answer=None)
    assert result.option_id == "allow"


@pytest.mark.asyncio
async def test_wait_for_answer_applies_default_on_timeout():
    at = AskTracker()
    q = _allow_deny()  # 50ms timeout, default deny
    cid = await _register_question(at, q)
    result = await at.wait_for_answer(
        cid, timeout_seconds=q.timeout_seconds, default_answer=q.default_answer,
    )
    assert result.option_id == "deny"
    assert result.outcome == "timed_out"
    # the timeout answer is recorded on the ask (ledger stays truthful)
    ask = await at.get(cid)
    assert ask is not None and ask.answer is not None and ask.answer.option_id == "deny"


@pytest.mark.asyncio
async def test_wait_for_answer_returns_immediately_if_already_answered():
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())
    await at.answer(cid, Answer(option_id="allow"))
    result = await at.wait_for_answer(cid, timeout_seconds=1.0, default_answer=None)
    assert result.option_id == "allow"


@pytest.mark.asyncio
async def test_text_answer_also_lands_as_reply_text():
    at = AskTracker()
    cid = await at.register(
        from_peer_id="a", from_peer_name="a", to_peer_id="b", to_peer_name="b",
        text="what's the branch?", question=Question(kind="text"),
    )
    ask = await at.answer(cid, Answer(text="main"))
    assert ask.answer.text == "main"
    assert ask.reply_text == "main"  # text answers populate reply_text too


@pytest.mark.asyncio
async def test_evicting_ask_cancels_a_dangling_waiter():
    # A blocking transport awaiting an ask that gets forgotten/evicted must not
    # hang — the waiter resolves with a cancelled Answer.
    at = AskTracker()
    cid = await at.register(
        from_peer_id="agent", from_peer_name="agent",
        to_peer_id="human", to_peer_name="human",
        text="?", question=Question(kind="text"),
    )

    async def forget_soon():
        await asyncio.sleep(0.01)
        await at.forget_peer("human")

    asyncio.create_task(forget_soon())
    # no timeout — would hang forever without waiter cancellation
    result = await at.wait_for_answer(cid, timeout_seconds=None, default_answer=None)
    assert result.outcome == "cancelled"


@pytest.mark.asyncio
async def test_wait_for_answer_rejects_invalid_default_loudly():
    # codex review: an invalid default_answer must fail loud at the call, not
    # silently return unrecorded at timeout (which would break the ledger).
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())
    bad = Answer(option_id="not-an-option", outcome="answered")
    with pytest.raises(ValueError, match="invalid default_answer"):
        await at.wait_for_answer(cid, timeout_seconds=0.01, default_answer=bad)


@pytest.mark.asyncio
async def test_close_cancels_a_dangling_waiter():
    # close() via a non-answer terminal path (ack/send_failed/reply_to) must not
    # leave a blocking transport waiting forever (codex review).
    at = AskTracker()
    cid = await _register_question(at, _allow_deny())

    async def close_soon():
        await asyncio.sleep(0.01)
        await at.close(cid, reason="send_failed")

    asyncio.create_task(close_soon())
    result = await at.wait_for_answer(cid, timeout_seconds=None, default_answer=None)
    assert result.outcome == "cancelled"
    assert result.message == "send_failed"


@pytest.mark.asyncio
async def test_plain_ask_without_question_is_unaffected():
    at = AskTracker()
    cid = await at.register(
        from_peer_id="a", from_peer_name="a", to_peer_id="b", to_peer_name="b", text="hi",
    )
    ask = await at.get(cid)
    assert ask is not None and ask.question is None and ask.answer is None
