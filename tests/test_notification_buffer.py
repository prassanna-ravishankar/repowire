"""Tests for buffering notify/broadcast while a peer is BUSY.

Issue #79: messages injected during a busy turn land in the active subagent's
context instead of the human's main session. The fix buffers at the registry
and drains on BUSY -> ONLINE.
"""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.peers import Peer, PeerStatus


@pytest.fixture
def router():
    r = MagicMock(spec=MessageRouter)
    r.send_query = AsyncMock(return_value="mock")
    r.send_notification = AsyncMock()
    r.send_broadcast_to = AsyncMock()
    r.broadcast = AsyncMock(return_value=[])
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


class TestNotifyBuffersWhenBusy:
    async def test_notify_to_busy_peer_is_queued(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(
            from_peer=sender.display_name,
            to_peer=recipient.display_name,
            text="hi",
        )

        router.send_notification.assert_not_awaited()
        assert registry._pending_messages[recipient.peer_id] == [
            {"kind": "notify", "from_peer": sender.display_name, "text": "hi"},
        ]

    async def test_notify_to_online_peer_is_sent_directly(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.ONLINE)

        await registry.notify(
            from_peer=sender.display_name,
            to_peer=recipient.display_name,
            text="hi",
        )

        router.send_notification.assert_awaited_once()
        assert recipient.peer_id not in registry._pending_messages

    async def test_busy_to_online_drains_in_order(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(sender.display_name, recipient.display_name, "first")
        await registry.notify(sender.display_name, recipient.display_name, "second")
        await registry.notify(sender.display_name, recipient.display_name, "third")

        router.send_notification.assert_not_awaited()

        await registry.update_peer_status(recipient.peer_id, PeerStatus.ONLINE)

        assert router.send_notification.await_count == 3
        # Preserve FIFO order
        texts = [c.kwargs["text"] for c in router.send_notification.await_args_list]
        assert texts == ["first", "second", "third"]
        # Queue cleared after drain
        assert recipient.peer_id not in registry._pending_messages

    async def test_busy_to_offline_drops_queue(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(sender.display_name, recipient.display_name, "lost")
        assert recipient.peer_id in registry._pending_messages

        await registry.update_peer_status(recipient.peer_id, PeerStatus.OFFLINE)

        router.send_notification.assert_not_awaited()
        assert recipient.peer_id not in registry._pending_messages

    async def test_status_unchanged_does_not_drain(self, registry, router):
        # BUSY -> BUSY (no transition) should not trigger a flush.
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(sender.display_name, recipient.display_name, "queued")

        await registry.update_peer_status(recipient.peer_id, PeerStatus.BUSY)

        router.send_notification.assert_not_awaited()
        assert registry._pending_messages[recipient.peer_id]

    async def test_queue_failure_does_not_block_subsequent_messages(self, registry, router):
        # If a single flushed send raises, the rest of the queue must still go.
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.notify(sender.display_name, recipient.display_name, "first")
        await registry.notify(sender.display_name, recipient.display_name, "second")

        call_count = 0

        async def maybe_fail(**kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                raise RuntimeError("transport flapped")

        router.send_notification.side_effect = maybe_fail

        await registry.update_peer_status(recipient.peer_id, PeerStatus.ONLINE)

        # Both attempted, second still went through
        assert router.send_notification.await_count == 2

    async def test_queue_caps_at_max(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        recipient = _add_peer(registry, "bob", PeerStatus.BUSY)

        registry._pending_messages_max = 3
        for i in range(5):
            await registry.notify(sender.display_name, recipient.display_name, f"msg-{i}")

        queue = registry._pending_messages[recipient.peer_id]
        assert len(queue) == 3
        # Oldest dropped, newest preserved
        assert [m["text"] for m in queue] == ["msg-2", "msg-3", "msg-4"]


class TestBroadcastBuffersBusyRecipients:
    async def test_busy_recipient_queued_not_sent(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        _add_peer(registry, "bob", PeerStatus.ONLINE)
        busy = _add_peer(registry, "carol", PeerStatus.BUSY)

        # Make sure the live MessageRouter.broadcast skips the busy peer.
        # Our router mock returns []; we just check the exclude set passed.
        await registry.broadcast(from_peer=sender.display_name, text="all hands")

        # busy peer was queued
        assert registry._pending_messages[busy.peer_id] == [
            {"kind": "broadcast", "from_peer": sender.display_name, "text": "all hands"},
        ]

        # Excluded from the live fanout
        exclude = router.broadcast.await_args.kwargs["exclude"]
        assert busy.peer_id in exclude

    async def test_drain_uses_send_broadcast_to(self, registry, router):
        sender = _add_peer(registry, "alice", PeerStatus.ONLINE)
        busy = _add_peer(registry, "bob", PeerStatus.BUSY)

        await registry.broadcast(from_peer=sender.display_name, text="hi")
        await registry.update_peer_status(busy.peer_id, PeerStatus.ONLINE)

        router.send_broadcast_to.assert_awaited_once()
        kwargs = router.send_broadcast_to.await_args.kwargs
        assert kwargs["text"] == "hi"
        assert kwargs["to_session_id"] == busy.peer_id
