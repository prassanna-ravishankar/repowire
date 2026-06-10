"""wait_on_ack primitive: bounded, non-recording waits with pull reply delivery."""

from __future__ import annotations

import asyncio

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.routes import asks as asks_routes
from repowire.protocol.questions import Answer
from tests.conftest import async_client_for, make_daemon_app


async def _open_ask(at: AskTracker, *, reply_delivery: str = "push") -> str:
    return await at.register(
        from_peer_id="repow-asker",
        from_peer_name="asker",
        to_peer_id="repow-replier",
        to_peer_name="replier",
        text="question?",
        reply_delivery=reply_delivery,
    )


@pytest.mark.asyncio
async def test_wait_resolves_when_answered():
    at = AskTracker()
    cid = await _open_ask(at)

    async def answer_soon():
        await asyncio.sleep(0.05)
        await at.answer(cid, Answer(outcome="answered", text="here you go"))

    answering = asyncio.create_task(answer_soon())
    ask = await at.wait_for_resolution(cid, timeout_seconds=5.0)
    await answering

    assert ask is not None and ask.closed
    assert ask.answer is not None and ask.answer.text == "here you go"


@pytest.mark.asyncio
async def test_wait_timeout_records_nothing_and_ask_stays_open():
    at = AskTracker()
    cid = await _open_ask(at)

    result = await at.wait_for_resolution(cid, timeout_seconds=0.05)

    assert result is None
    ask = await at.get(cid)
    assert ask is not None and not ask.closed and ask.answer is None
    # A later real answer still lands.
    await at.answer(cid, Answer(outcome="answered", text="late but real"))
    resolved = await at.wait_for_resolution(cid, timeout_seconds=0.05)
    assert resolved is not None and resolved.answer.text == "late but real"


@pytest.mark.asyncio
async def test_wait_switches_ask_to_pull_delivery():
    at = AskTracker()
    cid = await _open_ask(at)

    await at.wait_for_resolution(cid, timeout_seconds=0.01)

    ask = await at.get(cid)
    assert ask is not None and ask.reply_delivery == "pull"


@pytest.mark.asyncio
async def test_wait_resolves_on_close():
    at = AskTracker()
    cid = await _open_ask(at)

    async def close_soon():
        await asyncio.sleep(0.05)
        await at.close(cid, reason="ack")

    closing = asyncio.create_task(close_soon())
    ask = await at.wait_for_resolution(cid, timeout_seconds=5.0)
    await closing

    assert ask is not None and ask.closed and ask.close_reason == "ack"


@pytest.mark.asyncio
async def test_wait_unknown_cid_raises():
    at = AskTracker()
    with pytest.raises(KeyError):
        await at.wait_for_resolution("ask-nope", timeout_seconds=0.01)


@pytest.mark.asyncio
async def test_wait_route_resolves_and_404s(tmp_path):
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    at = harness.ask_tracker
    cid = await _open_ask(at)
    await at.capture_reply(cid, "the reply body")
    await at.close(cid, reason="ack_with_msg")

    async with async_client_for(harness.app) as client:
        resp = await client.post(f"/asks/{cid}/wait", json={"timeout_seconds": 1})
        assert resp.status_code == 200
        body = resp.json()
        assert body["status"] == "resolved"
        assert body["reply"] == "the reply body"
        assert body["close_reason"] == "ack_with_msg"

        missing = await client.post("/asks/ask-nope/wait", json={})
        assert missing.status_code == 404


@pytest.mark.asyncio
async def test_wait_route_pending_on_timeout(tmp_path):
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    cid = await _open_ask(harness.ask_tracker)

    async with async_client_for(harness.app) as client:
        resp = await client.post(f"/asks/{cid}/wait", json={"timeout_seconds": 0.05})

    assert resp.status_code == 200
    assert resp.json()["status"] == "pending"
    ask = await harness.ask_tracker.get(cid)
    assert ask is not None and not ask.closed


@pytest.mark.asyncio
async def test_pull_mode_ack_skips_notify_and_captures_reply(tmp_path):
    """An ack with message on a pull-delivery ask must not require the asker
    to have a live transport: the reply is retained on the ask and the open
    wait resolves with it (the wait_on_ack contract)."""
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    at = harness.ask_tracker
    cid = await _open_ask(at, reply_delivery="pull")

    waiter = asyncio.create_task(at.wait_for_resolution(cid, timeout_seconds=5.0))
    await asyncio.sleep(0)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/ack", json={"correlation_id": cid, "message": "review done: LGTM"},
        )
    assert resp.status_code == 200

    ask = await asyncio.wait_for(waiter, timeout=5.0)
    assert ask is not None and ask.closed
    assert ask.close_reason == "ack_with_msg"
    assert ask.reply_text == "review done: LGTM"
