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
from repowire.daemon.routes import acp_permissions
from repowire.daemon.websocket_transport import WebSocketTransport


@pytest.mark.asyncio
async def test_approval_broker_timeout_emits_default_deny_events() -> None:
    events: list[dict] = []

    def _emit(event_type: str, data: dict) -> str:
        events.append({"type": event_type, **data})
        return f"evt-{len(events)}"

    broker = ApprovalBroker(emit_event=_emit, timeout_seconds=0.01)

    decision = await broker.request_permission(
        peer_id="peer-1",
        session_id="session-1",
        tool_call={"name": "shell", "tool_call_id": "call-1"},
        options=[SimpleNamespace(option_id="opt-allow", title="Allow")],
    )

    assert decision.outcome == "denied"
    assert decision.timed_out is True
    assert events[0]["type"] == "acp_permission_request"
    assert events[0]["peer_id"] == "peer-1"
    assert events[0]["session_id"] == "session-1"
    assert events[0]["tool_call"]["name"] == "shell"
    assert events[0]["options"] == [{"option_id": "opt-allow", "title": "Allow"}]
    assert events[0]["status"] == "pending"
    assert events[1]["type"] == "acp_permission_decision"
    assert events[1]["request_id"] == events[0]["request_id"]
    assert events[1]["outcome"] == "denied"
    assert events[1]["status"] == "timed_out"


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

    broker = ApprovalBroker(emit_event=registry.add_event, timeout_seconds=5.0)
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
    return app, broker, registry


async def _wait_for_event(registry: PeerRegistry, event_type: str) -> dict:
    deadline = time.monotonic() + 1.0
    while time.monotonic() < deadline:
        matches = [event for event in registry.get_events() if event["type"] == event_type]
        if matches:
            return matches[-1]
        await asyncio.sleep(0.01)
    raise AssertionError(f"timed out waiting for {event_type}")
