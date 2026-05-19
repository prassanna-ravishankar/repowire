from __future__ import annotations

import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

from repowire.acp import AcpPromptResult
from repowire.acp.models import AcpPeerConfig
from repowire.config.models import Config
from repowire.daemon.transport_router import (
    AskEnvelope,
    NotifyEnvelope,
    PeerTransportRouter,
)
from repowire.protocol.peers import Peer, PeerStatus


def _config(*, acp_enabled: bool) -> Config:
    config = Config()
    config.experiments.acp_broker_client = acp_enabled
    return config


def _acp_peer() -> Peer:
    peer = Peer(
        peer_id="target-id",
        display_name="target-codex",
        path="/tmp/target",
        machine="localhost",
        status=PeerStatus.ONLINE,
    )
    peer.metadata["acp"] = AcpPeerConfig(command="python", cwd="/tmp").model_dump()
    return peer


def _ask_envelope(target: Peer) -> AskEnvelope:
    return AskEnvelope(
        from_peer_id="sender-id",
        from_peer_name="sender-codex",
        target=target,
        text="question",
        correlation_id="cid-1",
        intended_recipient_name=target.display_name,
    )


def _notify_envelope(target: Peer) -> NotifyEnvelope:
    return NotifyEnvelope(
        from_peer_id="sender-id",
        from_peer_name="sender-codex",
        target=target,
        text="heads up",
        intended_recipient_name=target.display_name,
    )


async def _wait_for_await(mock: AsyncMock) -> None:
    for _ in range(20):
        if mock.await_count:
            return
        await asyncio.sleep(0)
    raise AssertionError("mock was not awaited")


@pytest.mark.asyncio
async def test_ask_routes_to_acp_before_websocket() -> None:
    target = _acp_peer()
    registry = SimpleNamespace(add_event=Mock())
    ws_router = SimpleNamespace(send_ask=AsyncMock(), send_notification=AsyncMock())
    acp_manager = SimpleNamespace(
        prompt=AsyncMock(return_value=AcpPromptResult(stop_reason="end_turn", text="answer")),
    )
    on_complete = AsyncMock()

    router = PeerTransportRouter(
        config=_config(acp_enabled=True),
        registry=registry,
        message_router=ws_router,
        acp_manager=acp_manager,
    )
    await router.send_ask(_ask_envelope(target), on_acp_complete=on_complete)

    await _wait_for_await(acp_manager.prompt)
    acp_manager.prompt.assert_awaited_once()
    ws_router.send_ask.assert_not_awaited()
    await _wait_for_await(on_complete)
    on_complete.assert_awaited_once_with("cid-1", "answer", None)


@pytest.mark.asyncio
async def test_notify_routes_to_acp_and_discards_reply() -> None:
    target = _acp_peer()
    registry = SimpleNamespace(add_event=Mock())
    ws_router = SimpleNamespace(send_ask=AsyncMock(), send_notification=AsyncMock())
    acp_manager = SimpleNamespace(
        prompt=AsyncMock(
            return_value=AcpPromptResult(stop_reason="end_turn", text="discarded"),
        ),
    )

    router = PeerTransportRouter(
        config=_config(acp_enabled=True),
        registry=registry,
        message_router=ws_router,
        acp_manager=acp_manager,
    )
    status = await router.send_notify(_notify_envelope(target))

    assert status == "sent"
    await _wait_for_await(acp_manager.prompt)
    acp_manager.prompt.assert_awaited_once()
    ws_router.send_notification.assert_not_awaited()


@pytest.mark.asyncio
async def test_flag_off_acp_peer_falls_through_to_websocket() -> None:
    target = _acp_peer()
    registry = SimpleNamespace(add_event=Mock())
    ws_router = SimpleNamespace(send_ask=AsyncMock(), send_notification=AsyncMock())
    acp_manager = SimpleNamespace(
        prompt=AsyncMock(return_value=AcpPromptResult(stop_reason="end_turn", text="unused")),
    )
    router = PeerTransportRouter(
        config=_config(acp_enabled=False),
        registry=registry,
        message_router=ws_router,
        acp_manager=acp_manager,
    )

    await router.send_ask(_ask_envelope(target), on_acp_complete=AsyncMock())
    status = await router.send_notify(_notify_envelope(target))

    assert status == "sent"
    acp_manager.prompt.assert_not_awaited()
    ws_router.send_ask.assert_awaited_once()
    ws_router.send_notification.assert_awaited_once()
