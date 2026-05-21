from __future__ import annotations

import asyncio
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon import app as app_mod
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore


async def _register_peer(
    client: AsyncClient,
    *,
    name: str,
    path: str,
    pane_id: str,
    runtime_session_id: str,
) -> tuple[str, str]:
    response = await client.post(
        "/peers",
        json={
            "name": name,
            "path": path,
            "circle": "default",
            "backend": "claude-code",
            "pane_id": pane_id,
            "metadata": {"hook_session_id": runtime_session_id},
        },
    )
    assert response.status_code == 200, response.text
    body = response.json()
    return body["peer_id"], body["display_name"]


@pytest.mark.asyncio
async def test_ask_ack_and_notify_events_carry_session_bindings(tmp_path: Path) -> None:
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with app.router.lifespan_context(app):
        assert isinstance(app.state.session_binding_store, SQLiteSessionBindingStore)
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            alice_id, alice_name = await _register_peer(
                client,
                name="alice",
                path="/repo/alice",
                pane_id="%11",
                runtime_session_id="runtime-alice",
            )
            bob_id, bob_name = await _register_peer(
                client,
                name="bob",
                path="/repo/bob",
                pane_id="%12",
                runtime_session_id="runtime-bob",
            )
            alice_binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-alice",
                backend="claude-code",
                project_path="/repo/alice",
            )
            bob_binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-bob",
                backend="claude-code",
                project_path="/repo/bob",
            )
            assert alice_binding is not None
            assert bob_binding is not None

            app.state.peer_registry._router.send_ask = AsyncMock()
            ask_response = await client.post(
                "/ask",
                json={
                    "from_peer": alice_name,
                    "to_peer": bob_name,
                    "text": "question",
                },
            )
            assert ask_response.status_code == 200, ask_response.text
            correlation_id = ask_response.json()["correlation_id"]

            events = app.state.peer_registry.get_events()
            ask_event = [event for event in events if event["type"] == "ask"][-1]
            assert ask_event["from_peer_id"] == alice_id
            assert ask_event["to_peer_id"] == bob_id
            assert ask_event["repowire_session_id"] == bob_binding.repowire_session_id
            assert ask_event["from_repowire_session_id"] == alice_binding.repowire_session_id
            assert ask_event["to_repowire_session_id"] == bob_binding.repowire_session_id

            hook_ack = {
                "type": "delivery_ack",
                "delivery_id": "notif-delivery-test",
                "message_type": "notify",
                "status": "injected",
            }
            app.state.peer_registry._router.send_notification = AsyncMock(
                return_value=hook_ack,
            )
            notify_response = await client.post(
                "/notify",
                json={
                    "from_peer": alice_name,
                    "to_peer": bob_name,
                    "text": "FYI",
                },
            )
            assert notify_response.status_code == 200, notify_response.text
            notify_body = notify_response.json()
            assert notify_body["repowire_session_id"] == bob_binding.repowire_session_id
            assert (
                notify_body["hook_delivery"]["repowire_session_id"]
                == bob_binding.repowire_session_id
            )
            assert (
                notify_body["hook_delivery"]["from_repowire_session_id"]
                == alice_binding.repowire_session_id
            )

            ack_response = await client.post(
                "/ack",
                json={
                    "correlation_id": correlation_id,
                    "message": "answer",
                },
            )
            assert ack_response.status_code == 200, ack_response.text
            ack_reply_event = [
                event for event in app.state.peer_registry.get_events()
                if event["type"] == "notification"
            ][-1]
            assert ack_reply_event["from_peer_id"] == bob_id
            assert ack_reply_event["to_peer_id"] == alice_id
            assert (
                ack_reply_event["repowire_session_id"]
                == alice_binding.repowire_session_id
            )
            assert (
                ack_reply_event["from_repowire_session_id"]
                == bob_binding.repowire_session_id
            )
            assert (
                ack_reply_event["to_repowire_session_id"]
                == alice_binding.repowire_session_id
            )

            app.state.peer_registry._router.send_ask = AsyncMock()
            bare_ask_response = await client.post(
                "/ask",
                json={
                    "from_peer": alice_name,
                    "to_peer": bob_name,
                    "text": "close only",
                },
            )
            assert bare_ask_response.status_code == 200, bare_ask_response.text
            bare_ack_response = await client.post(
                "/ack",
                json={"correlation_id": bare_ask_response.json()["correlation_id"]},
            )
            assert bare_ack_response.status_code == 200, bare_ack_response.text
            ack_event = [
                event for event in app.state.peer_registry.get_events()
                if event["type"] == "ack"
            ][-1]
            assert ack_event["repowire_session_id"] == alice_binding.repowire_session_id
            assert ack_event["from_repowire_session_id"] == bob_binding.repowire_session_id
            assert ack_event["to_repowire_session_id"] == alice_binding.repowire_session_id


@pytest.mark.asyncio
async def test_detached_sender_leaves_ask_sender_binding_null(tmp_path: Path) -> None:
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with app.router.lifespan_context(app):
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            _, bob_name = await _register_peer(
                client,
                name="bob",
                path="/repo/bob",
                pane_id="%22",
                runtime_session_id="runtime-bob",
            )
            bob_binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-bob",
                backend="claude-code",
                project_path="/repo/bob",
            )
            assert bob_binding is not None

            app.state.peer_registry._router.send_ask = AsyncMock()
            response = await client.post(
                "/ask",
                json={
                    "from_peer": "detached-cli",
                    "to_peer": bob_name,
                    "text": "offline sender",
                    "bypass_circle": True,
                },
            )
            assert response.status_code == 200, response.text

            ask_event = [
                event for event in app.state.peer_registry.get_events()
                if event["type"] == "ask"
            ][-1]
            assert ask_event["from_peer_id"] == "detached-cli"
            assert ask_event["from_repowire_session_id"] is None
            assert ask_event["to_repowire_session_id"] == bob_binding.repowire_session_id


@pytest.mark.asyncio
async def test_acp_permission_events_carry_session_binding(tmp_path: Path) -> None:
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with app.router.lifespan_context(app):
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            peer_id, _ = await _register_peer(
                client,
                name="worker",
                path="/repo/worker",
                pane_id="%33",
                runtime_session_id="runtime-worker",
            )
            binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-worker",
                backend="claude-code",
                project_path="/repo/worker",
            )
            assert binding is not None
            broker = app.state.acp_permission_broker

            task = asyncio.create_task(
                broker.request_permission(
                    peer_id=peer_id,
                    session_id="runtime-worker",
                    tool_call={"name": "write_file"},
                    options=[SimpleNamespace(option_id="allow", title="Allow")],
                )
            )
            request_event = await _wait_for_event(
                app.state.peer_registry,
                "acp_permission_request",
            )
            assert request_event["repowire_session_id"] == binding.repowire_session_id

            response = await client.post(
                f"/acp/permissions/{request_event['request_id']}/decision",
                json={"outcome": "allowed", "option_id": "allow"},
            )
            assert response.status_code == 200, response.text
            await asyncio.wait_for(task, timeout=1.0)

            decision_event = await _wait_for_event(
                app.state.peer_registry,
                "acp_permission_decision",
            )
            assert decision_event["repowire_session_id"] == binding.repowire_session_id


async def _wait_for_event(registry, event_type: str) -> dict:
    deadline = time.monotonic() + 1.0
    while time.monotonic() < deadline:
        matches = [event for event in registry.get_events() if event["type"] == event_type]
        if matches:
            return matches[-1]
        await asyncio.sleep(0.01)
    raise AssertionError(f"timed out waiting for {event_type}")
