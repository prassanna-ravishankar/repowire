from __future__ import annotations

import asyncio
import time
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.acp import ApprovalBroker
from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import acp_permissions, asks
from repowire.daemon.websocket_transport import WebSocketTransport


@pytest.mark.asyncio
async def test_approval_broker_timeout_emits_default_deny_events() -> None:
    events: list[dict] = []

    def _emit(event_type: str, data: dict) -> str:
        events.append({"type": event_type, **data})
        return f"evt-{len(events)}"

    broker = ApprovalBroker(
        emit_event=_emit, ask_tracker=AskTracker(ttl_hours=24.0), timeout_seconds=0.01,
    )

    decision = await broker.request_permission(
        peer_id="peer-1",
        session_id="session-1",
        tool_call={"name": "shell", "tool_call_id": "call-1"},
        options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
    )

    assert decision.outcome == "denied"
    assert decision.timed_out is True
    # A normal ask event (for the dashboard/Telegram renderers) AND the
    # back-compat ACP-specific aliases are emitted, keyed by the same request_id.
    ask_evt = next(e for e in events if e["type"] == "ask")
    assert ask_evt["to_peer_id"] == "__repowire_control__"
    assert ask_evt["question"]["scope"] == "tool_permission"
    req = next(e for e in events if e["type"] == "acp_permission_request")
    assert req["peer_id"] == "peer-1"
    assert req["session_id"] == "session-1"
    assert req["tool_call"]["name"] == "shell"
    assert req["options"] == [{"option_id": "opt-allow", "title": "Allow"}]
    assert req["status"] == "pending"
    dec = next(e for e in events if e["type"] == "acp_permission_decision")
    assert dec["request_id"] == req["request_id"]
    assert dec["outcome"] == "denied"
    assert dec["status"] == "timed_out"


@pytest.mark.asyncio
async def test_permission_decision_route_resolves_pending_request(tmp_path: Path) -> None:
    app, broker, registry = _make_permission_app(tmp_path)
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1",
                session_id="session-1",
                tool_call={"name": "write_file"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow write")],
            )
        )
        request_event = await _wait_for_event(registry, "acp_permission_request")

        async with AsyncClient(
            transport=ASGITransport(app=app),
            base_url="http://test",
        ) as http:
            response = await http.post(
                f"/acp/permissions/{request_event['request_id']}/decision",
                json={
                    "outcome": "allowed",
                    "option_id": "opt-allow",
                    "message": "approved by test",
                },
            )

        assert response.status_code == 200, response.text
        assert response.json() == {
            "ok": True,
            "request_id": request_event["request_id"],
            "outcome": "allowed",
            "option_id": "opt-allow",
        }

        decision = await asyncio.wait_for(task, timeout=1.0)
        assert decision.outcome == "allowed"
        assert decision.option_id == "opt-allow"

        decision_event = await _wait_for_event(registry, "acp_permission_decision")
        assert decision_event["request_id"] == request_event["request_id"]
        assert decision_event["peer_id"] == "peer-1"
        assert decision_event["session_id"] == "session-1"
        assert decision_event["outcome"] == "allowed"
        assert decision_event["option_id"] == "opt-allow"
        assert decision_event["status"] == "decided"
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_permission_decision_route_rejects_unknown_option(tmp_path: Path) -> None:
    app, broker, registry = _make_permission_app(tmp_path)
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1",
                session_id="session-1",
                tool_call={"name": "write_file"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow write")],
            )
        )
        request_event = await _wait_for_event(registry, "acp_permission_request")

        async with AsyncClient(
            transport=ASGITransport(app=app),
            base_url="http://test",
        ) as http:
            response = await http.post(
                f"/acp/permissions/{request_event['request_id']}/decision",
                json={"outcome": "allowed", "option_id": "wrong-option"},
            )

        assert response.status_code == 400
        await broker.decide(request_event["request_id"], outcome="cancelled")
        await task
    finally:
        cleanup_deps()


def _make_permission_app(tmp_path: Path) -> tuple[FastAPI, ApprovalBroker, PeerRegistry]:
    cfg = Config()
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=qt,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    broker = ApprovalBroker(emit_event=registry.add_event, ask_tracker=at, timeout_seconds=5.0)
    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
        acp_permission_broker=broker,
    )
    init_deps(cfg, registry, state)

    app = FastAPI()
    app.include_router(acp_permissions.router)
    app.include_router(asks.router)  # for the generic /answer verb
    return app, broker, registry


async def _wait_for_event(registry: PeerRegistry, event_type: str) -> dict:
    deadline = time.monotonic() + 1.0
    while time.monotonic() < deadline:
        matches = [event for event in registry.get_events() if event["type"] == event_type]
        if matches:
            return matches[-1]
        await asyncio.sleep(0.01)
    raise AssertionError(f"timed out waiting for {event_type}")


@pytest.mark.asyncio
async def test_acp_permission_resolves_via_generic_answer(tmp_path: Path) -> None:
    # The keystone: an ACP permission request IS a question ask, so answering it
    # through the generic /answer route (as a human/peer would) resolves the ACP
    # PermissionDecision — no ACP-specific decide call needed.
    app, broker, registry = _make_permission_app(tmp_path)
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1", session_id="session-1",
                tool_call={"name": "shell"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
            )
        )
        req = await _wait_for_event(registry, "acp_permission_request")
        cid = req["request_id"]

        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            # answer via the generic verb, picking the allow option
            r = await http.post("/answer", json={"correlation_id": cid, "option_id": "opt-allow"})
            assert r.status_code == 200, r.text

        decision = await asyncio.wait_for(task, timeout=1.0)
        assert decision.outcome == "allowed"
        assert decision.option_id == "opt-allow"
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_acp_permission_answer_fails_closed_on_bare_ack(tmp_path: Path) -> None:
    # Acknowledging a tool-permission question (no option selected) must DENY,
    # never allow — fail closed.
    app, broker, registry = _make_permission_app(tmp_path)
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1", session_id="session-1",
                tool_call={"name": "shell"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
            )
        )
        req = await _wait_for_event(registry, "acp_permission_request")
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            r = await http.post(
                "/answer", json={"correlation_id": req["request_id"], "outcome": "acknowledged"},
            )
            assert r.status_code == 200, r.text
        decision = await asyncio.wait_for(task, timeout=1.0)
        assert decision.outcome == "denied"
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_acp_permission_denied_via_first_class_outcome(tmp_path: Path) -> None:
    # POST /answer {outcome: "denied"} on a tool-permission → PermissionDecision(denied),
    # without needing a deny OPTION (ACP options are allow-options).
    app, broker, registry = _make_permission_app(tmp_path)
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1", session_id="session-1", tool_call={"name": "shell"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
            )
        )
        req = await _wait_for_event(registry, "acp_permission_request")
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            r = await http.post(
                "/answer", json={"correlation_id": req["request_id"], "outcome": "denied"},
            )
            assert r.status_code == 200, r.text
        decision = await asyncio.wait_for(task, timeout=1.0)
        assert decision.outcome == "denied"
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_tool_permission_answer_does_not_notify_runtime(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Answering a tool-permission must NOT notify the ACP "asker" — the runtime
    # already gets the decision via wait_for_answer; a notify would be a
    # redundant/intrusive prompt (codex review).
    from unittest.mock import AsyncMock

    import repowire.daemon.routes.asks as asks_mod

    app, broker, registry = _make_permission_app(tmp_path)
    notify_spy = AsyncMock()
    try:
        task = asyncio.create_task(
            broker.request_permission(
                peer_id="peer-1", session_id="session-1", tool_call={"name": "shell"},
                options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
            )
        )
        req = await _wait_for_event(registry, "acp_permission_request")
        # patch peer_delivery so any notify attempt is observable
        monkeypatch.setattr(
            asks_mod, "peer_delivery_from_state",
            lambda **_: SimpleNamespace(notify=notify_spy),
        )
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            r = await http.post(
                "/answer", json={"correlation_id": req["request_id"], "option_id": "opt-allow"},
            )
            assert r.status_code == 200, r.text
        decision = await asyncio.wait_for(task, timeout=1.0)
        assert decision.outcome == "allowed"
        notify_spy.assert_not_awaited()  # no runtime notify for a tool-permission answer
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_register_failure_emits_balancing_decision_event() -> None:
    # If the shared core's registration fails, the pending request alias must be
    # balanced by a decision event so a consumer never sees a dangling request.
    events: list[dict] = []

    class BoomTracker(AskTracker):
        async def register(self, *args, **kwargs):  # type: ignore[override]
            raise RuntimeError("register boom")

    broker = ApprovalBroker(
        emit_event=lambda t, d: events.append({"type": t, **d}) or "evt",
        ask_tracker=BoomTracker(ttl_hours=24.0),
        timeout_seconds=5.0,
    )

    decision = await broker.request_permission(
        peer_id="peer-1",
        session_id="session-1",
        tool_call={"name": "shell"},
        options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
    )

    assert decision.outcome == "denied"
    assert any(e["type"] == "acp_permission_request" for e in events)
    dec = next(e for e in events if e["type"] == "acp_permission_decision")
    assert dec["outcome"] == "denied"


@pytest.mark.asyncio
async def test_acp_permission_cancellation_cleans_pending_context() -> None:
    tracker = AskTracker(ttl_hours=24.0)
    broker = ApprovalBroker(
        emit_event=lambda _event_type, _data: "evt",
        ask_tracker=tracker,
        timeout_seconds=60.0,
    )
    task = asyncio.create_task(
        broker.request_permission(
            peer_id="peer-1",
            session_id="session-1",
            tool_call={"name": "shell"},
            options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
        )
    )
    deadline = time.monotonic() + 1.0
    while broker.health_snapshot()["pending"] == 0 and time.monotonic() < deadline:
        await asyncio.sleep(0.01)
    assert broker.health_snapshot()["pending"] == 1

    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    assert broker.health_snapshot()["pending"] == 0
