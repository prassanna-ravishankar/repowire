"""Blocking structured-question core + POST /questions/ask-blocking route.

repowire-qnp: the shared register-emit-wait core both the ACP broker and the
PreToolUse hook ride, and the transport-neutral HTTP seam that lets an external
suspendable caller (a hook) park on a question without a polling loop.
"""

from __future__ import annotations

import asyncio

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.question_blocking import (
    CONTROL_PEER_ID,
    register_blocking_question_and_wait,
)
from repowire.daemon.routes import asks as asks_routes
from repowire.protocol.questions import Answer, Question, QuestionOption

from .conftest import async_client_for, make_daemon_app


def _allow_question(timeout: float = 5.0) -> Question:
    return Question(
        kind="choice",
        prompt="Allow Bash?",
        options=[QuestionOption(id="allow", title="Allow Bash")],
        blocking=True,
        timeout_seconds=timeout,
        default_answer=Answer(outcome="denied", message="timed out"),
        scope="tool_permission",
    )


# --- shared core ---


@pytest.mark.asyncio
async def test_core_emits_ask_event_and_returns_recorded_answer():
    tracker = AskTracker(ttl_hours=24.0)
    events: list[tuple[str, dict]] = []

    async def answer_soon(cid: str):
        # Let the waiter register, then answer.
        for _ in range(50):
            if await tracker.get(cid) is not None:
                break
            await asyncio.sleep(0.005)
        await tracker.answer(cid, Answer(option_id="allow", outcome="answered"))

    answerer = asyncio.create_task(answer_soon("pretool-1"))
    answer = await register_blocking_question_and_wait(
        tracker,
        lambda t, d: events.append((t, d)) or "evt",
        question=_allow_question(),
        text="Allow Bash?",
        correlation_id="pretool-1",
        server_wait_seconds=5.0,
        from_peer_id="agent-x",
        from_peer_name="agent-x",
    )
    await answerer

    assert answer.outcome == "answered"
    assert answer.option_id == "allow"
    # The normal ask event fired so dashboard/Telegram render it, keyed to control.
    assert any(t == "ask" and d["correlation_id"] == "pretool-1" for t, d in events)
    ask_event = next(d for t, d in events if t == "ask")
    assert ask_event["to_peer_id"] == CONTROL_PEER_ID
    assert ask_event["question"]["scope"] == "tool_permission"


@pytest.mark.asyncio
async def test_core_timeout_records_default_answer_fail_closed():
    tracker = AskTracker(ttl_hours=24.0)
    answer = await register_blocking_question_and_wait(
        tracker,
        lambda _t, _d: "evt",
        question=_allow_question(timeout=0.05),
        text="Allow Bash?",
        correlation_id="pretool-timeout",
        server_wait_seconds=0.05,
        from_peer_id="agent-x",
        from_peer_name="agent-x",
    )
    assert answer.outcome == "denied"  # default_answer, fail closed


# --- route ---


@pytest.mark.asyncio
async def test_route_happy_path_returns_selected_option(tmp_path):
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    tracker = harness.ask_tracker

    async def answer_when_open():
        for _ in range(100):
            ask = await tracker.get("pretool-route-1")
            if ask is not None:
                break
            await asyncio.sleep(0.005)
        await tracker.answer("pretool-route-1", Answer(option_id="allow", outcome="answered"))

    async with async_client_for(harness.app) as client:
        answerer = asyncio.create_task(answer_when_open())
        resp = await client.post(
            "/questions/ask-blocking",
            json={
                "prompt": "Allow Bash?",
                "options": [{"id": "allow", "title": "Allow Bash"}],
                "scope": "tool_permission",
                "correlation_id": "pretool-route-1",
                "timeout_seconds": 5.0,
                "origin": "pretooluse",
            },
        )
        await answerer

    assert resp.status_code == 200
    body = resp.json()
    assert body["outcome"] == "answered"
    assert body["option_id"] == "allow"
    assert body["correlation_id"] == "pretool-route-1"


@pytest.mark.asyncio
async def test_route_timeout_denies_for_tool_permission(tmp_path):
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/questions/ask-blocking",
            json={
                "prompt": "Allow Bash?",
                "options": [{"id": "allow", "title": "Allow Bash"}],
                "scope": "tool_permission",
                "correlation_id": "pretool-route-timeout",
                "timeout_seconds": 0.05,
            },
        )
    assert resp.status_code == 200
    assert resp.json()["outcome"] == "denied"


@pytest.mark.asyncio
async def test_route_tool_permission_requires_an_option(tmp_path):
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/questions/ask-blocking",
            json={"prompt": "Allow Bash?", "options": [], "scope": "tool_permission"},
        )
    assert resp.status_code == 422


@pytest.mark.asyncio
async def test_route_clamps_timeout_to_daemon_max(tmp_path):
    # A huge requested timeout is clamped so a client can't pin a connection open.
    harness = make_daemon_app(tmp_path, [asks_routes.router])
    tracker = harness.ask_tracker

    async def answer_when_open():
        for _ in range(100):
            if await tracker.get("pretool-clamp") is not None:
                break
            await asyncio.sleep(0.005)
        ask = await tracker.get("pretool-clamp")
        assert ask is not None and ask.question is not None
        assert ask.question.timeout_seconds == asks_routes.BLOCKING_QUESTION_MAX_WAIT_SECONDS
        await tracker.answer("pretool-clamp", Answer(option_id="allow", outcome="answered"))

    async with async_client_for(harness.app) as client:
        answerer = asyncio.create_task(answer_when_open())
        resp = await client.post(
            "/questions/ask-blocking",
            json={
                "prompt": "Allow Bash?",
                "options": [{"id": "allow", "title": "Allow Bash"}],
                "scope": "tool_permission",
                "correlation_id": "pretool-clamp",
                "timeout_seconds": 99999.0,
            },
        )
        await answerer
    assert resp.status_code == 200
