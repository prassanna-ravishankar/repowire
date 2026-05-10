"""Tests confirming notify/ask/broadcast no longer buffer for BUSY peers.

The BUSY queue (formerly _pending_messages) was ripped: BUSY isn't a
delivery barrier under async ask semantics. Daemon sends directly via
the router; ws-hook buffers the tmux paste through the busy turn.
TransportError (no live WS) bubbles to the route as 503.
"""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.peers import Peer, PeerStatus


@pytest.fixture
def router():
    r = MagicMock(spec=MessageRouter)
    r.send_query = AsyncMock(return_value="mock")
    r.send_notification = AsyncMock()
    r.send_ask = AsyncMock()
    r.broadcast = AsyncMock(return_value=([], []))
    return r


@pytest.fixture
def registry(router):
    return PeerRegistry(config=Config(), message_router=router)


def _add_peer(
    registry: PeerRegistry,
    name: str,
    status: PeerStatus,
    circle: str = "global",
) -> Peer:
    peer = Peer(name=name, path="/x", machine="local", circle=circle, status=status)
    registry._peers[peer.peer_id] = peer
    return peer


class TestNotifyDirectSend:
    async def test_notify_to_busy_peer_sends_directly(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(
            from_peer=sender.display_name,
            to_peer=recipient.display_name,
        text="hi",
        )

        router.send_notification.assert_awaited_once()

    async def test_notify_to_online_peer_sends_directly(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.ONLINE)

        await registry.notify(
            from_peer=sender.display_name,
            to_peer=recipient.display_name,
            text="hi",
        )

        router.send_notification.assert_awaited_once()

    async def test_notify_propagates_transport_error(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        _add_peer(registry, "bob", PeerStatus.ONLINE)
        router.send_notification.side_effect = TransportError("No connection")

        with pytest.raises(TransportError):
            await registry.notify(
                from_peer=sender.display_name,
                to_peer="bob",
                text="hi",
            )


class TestAskDirectSend:
    async def test_deliver_ask_to_busy_peer_sends_directly(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.deliver_ask(
            from_peer=sender.display_name,
            to_peer=recipient.display_name,
            text="hi",
            correlation_id="ask-test",
        )

        router.send_ask.assert_awaited_once()

    async def test_deliver_ask_propagates_transport_error(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        _add_peer(registry, "bob", PeerStatus.ONLINE)
        router.send_ask.side_effect = TransportError("No connection")

        with pytest.raises(TransportError):
            await registry.deliver_ask(
                from_peer=sender.display_name,
                to_peer="bob",
                text="hi",
                correlation_id="ask-test",
            )


class TestBroadcastBestEffort:
    async def test_broadcast_excludes_self_and_circles(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE, circle="x")
        _add_peer(registry, "bob", PeerStatus.ONLINE, circle="x")
        _add_peer(registry, "carol", PeerStatus.ONLINE, circle="y")
        router.broadcast.return_value = ([], [])

        sent, failed = await registry.broadcast(
            from_peer=sender.display_name,
            text="hi",
        )

        # router.broadcast is called once with exclude including sender + cross-circle
        router.broadcast.assert_awaited_once()
        kwargs = router.broadcast.await_args.kwargs
        assert sender.peer_id in kwargs["exclude"]
        assert sent == []
        assert failed == []


class TestStatusUpdateNoSideEffects:
    async def test_busy_to_online_does_nothing_extra(self, registry, router):
        peer = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.update_peer_status(peer.display_name, PeerStatus.ONLINE)

        # No drain — router shouldn't be touched
        router.send_notification.assert_not_awaited()
        router.send_ask.assert_not_awaited()
        assert peer.status == PeerStatus.ONLINE
