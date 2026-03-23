"""Tests for MessageRouter — query, notification, and broadcast delivery."""

from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.daemon.message_router import MessageRouter
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.websocket_transport import TransportError, WebSocketTransport


@pytest.fixture
def transport():
    t = MagicMock(spec=WebSocketTransport)
    t.send = AsyncMock()
    t.is_connected = MagicMock(return_value=True)
    t.get_all_sessions = MagicMock(return_value=[])
    return t


@pytest.fixture
def tracker():
    return QueryTracker()


@pytest.fixture
def router(transport, tracker):
    return MessageRouter(transport=transport, query_tracker=tracker)


class TestSendNotification:
    async def test_sends_to_transport(self, router, transport):
        await router.send_notification("sender", "sid-1", "recipient", "hello")
        transport.send.assert_called_once()
        msg = transport.send.call_args[0][1]
        assert msg["type"] == "notify"
        assert msg["from_peer"] == "sender"
        assert msg["text"] == "hello"

    async def test_transport_error_propagates(self, router, transport):
        transport.send.side_effect = TransportError("disconnected")
        with pytest.raises(TransportError):
            await router.send_notification("sender", "sid-1", "recipient", "hello")


class TestSendQuery:
    async def test_not_connected_raises(self, router, transport):
        transport.is_connected.return_value = False
        with pytest.raises(ValueError, match="not connected"):
            await router.send_query("sender", "sid-1", "recipient", "hello?")

    async def test_sends_and_waits(self, router, transport, tracker):
        async def resolve_after_send(*args, **kwargs):
            # Simulate response arriving after send
            for cid in list(tracker._queries):
                await tracker.resolve_query(cid, "response!")

        transport.send.side_effect = resolve_after_send

        result = await router.send_query("sender", "sid-1", "recipient", "hello?")
        assert result == "response!"

    async def test_timeout(self, router, transport):
        with pytest.raises(TimeoutError):
            await router.send_query("sender", "sid-1", "recipient", "hello?", timeout=0.1)

    async def test_cleans_up_on_timeout(self, router, transport, tracker):
        try:
            await router.send_query("sender", "sid-1", "recipient", "hello?", timeout=0.1)
        except TimeoutError:
            pass
        assert tracker.get_pending_count() == 0


class TestBroadcast:
    async def test_broadcast_to_all(self, router, transport):
        transport.get_all_sessions.return_value = ["sid-1", "sid-2", "sid-3"]
        sent = await router.broadcast("sender", "hello all")
        assert len(sent) == 3
        assert transport.send.call_count == 3

    async def test_broadcast_excludes(self, router, transport):
        transport.get_all_sessions.return_value = ["sid-1", "sid-2", "sid-3"]
        sent = await router.broadcast("sender", "hello", exclude={"sid-2"})
        assert len(sent) == 2
        assert "sid-2" not in sent

    async def test_broadcast_empty(self, router, transport):
        transport.get_all_sessions.return_value = []
        sent = await router.broadcast("sender", "hello")
        assert sent == []

    async def test_broadcast_partial_failure(self, router, transport):
        transport.get_all_sessions.return_value = ["sid-1", "sid-2"]

        call_count = 0

        async def fail_second(sid, msg):
            nonlocal call_count
            call_count += 1
            if call_count == 2:
                raise TransportError("disconnected")

        transport.send.side_effect = fail_second
        sent = await router.broadcast("sender", "hello")
        assert len(sent) == 1
