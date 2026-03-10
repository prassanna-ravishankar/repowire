"""Tests for lazy_repair, get_peer_by_pane, and ping/pong liveness."""

from __future__ import annotations

import asyncio
import time
from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import AgentType, Config
from repowire.daemon.core import PeerManager
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerStatus


def _make_peer(
    peer_id: str = "repow-dev-abc12345",
    display_name: str = "myproject",
    status: PeerStatus = PeerStatus.ONLINE,
    backend: AgentType = AgentType.CLAUDE_CODE,
    pane_id: str | None = None,
    circle: str = "dev",
) -> Peer:
    return Peer(
        peer_id=peer_id,
        display_name=display_name,
        path="/tmp/test",
        machine="test",
        backend=backend,
        circle=circle,
        status=status,
        pane_id=pane_id,
    )


def _make_manager(
    transport: WebSocketTransport | None = None,
    query_tracker: MagicMock | None = None,
) -> PeerManager:
    config = Config()
    router = MagicMock()
    session_mapper = MagicMock()
    session_mapper.get_all_mappings.return_value = {}
    return PeerManager(
        config=config,
        message_router=router,
        session_mapper=session_mapper,
        query_tracker=query_tracker,
        transport=transport,
    )


# -- get_peer_by_pane --


class TestGetPeerByPane:
    @pytest.fixture
    def manager(self):
        return _make_manager()

    async def test_found(self, manager):
        peer = _make_peer(pane_id="%5")
        await manager.register_peer(peer)
        result = await manager.get_peer_by_pane("%5")
        assert result is not None
        assert result.peer_id == peer.peer_id

    async def test_not_found(self, manager):
        peer = _make_peer(pane_id="%5")
        await manager.register_peer(peer)
        result = await manager.get_peer_by_pane("%99")
        assert result is None

    async def test_no_pane_id(self, manager):
        peer = _make_peer(pane_id=None)
        await manager.register_peer(peer)
        result = await manager.get_peer_by_pane("%5")
        assert result is None


# -- lazy_repair debouncing --


class TestLazyRepairDebounce:
    async def test_second_call_within_30s_is_noop(self):
        transport = MagicMock(spec=WebSocketTransport)
        manager = _make_manager(transport=transport)

        peer = _make_peer()
        await manager.register_peer(peer)

        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(return_value={"type": "pong"})

        # First call runs repair
        await manager.lazy_repair()
        assert transport.is_connected.call_count == 1

        # Second call within 30s is a no-op
        transport.is_connected.reset_mock()
        await manager.lazy_repair()
        assert transport.is_connected.call_count == 0

    async def test_runs_after_debounce_expires(self):
        transport = MagicMock(spec=WebSocketTransport)
        manager = _make_manager(transport=transport)

        peer = _make_peer()
        await manager.register_peer(peer)

        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(return_value={"type": "pong"})

        await manager.lazy_repair()
        assert transport.is_connected.call_count == 1

        # Simulate 31s passing
        manager._last_repair = time.monotonic() - 31.0

        transport.is_connected.reset_mock()
        await manager.lazy_repair()
        assert transport.is_connected.call_count == 1


# -- liveness checks --


class TestLazyRepairLiveness:
    async def test_no_ws_marks_offline(self):
        """Peer with no WebSocket connection is marked OFFLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = MagicMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE

    async def test_pong_alive_stays_online(self):
        """Peer that responds to ping stays ONLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(return_value={"type": "pong"})
        manager = _make_manager(transport=transport)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE

    async def test_ping_timeout_marks_offline(self):
        """Peer that doesn't respond to ping is marked OFFLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(side_effect=TimeoutError("no pong"))
        qt = MagicMock()
        qt.cancel_queries_to_peer = MagicMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE

    async def test_opencode_skips_ping(self):
        """OpenCode peers skip ping — WS connected = alive."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(side_effect=AssertionError("should not be called"))
        manager = _make_manager(transport=transport)

        peer = _make_peer(
            peer_id="repow-dev-oc123456",
            backend=AgentType.OPENCODE,
            status=PeerStatus.ONLINE,
        )
        await manager.register_peer(peer)

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        transport.ping.assert_not_awaited()

    async def test_offline_peers_skipped(self):
        """OFFLINE peers are not checked during repair."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(side_effect=AssertionError("should not be called"))
        manager = _make_manager(transport=transport)

        peer = _make_peer(status=PeerStatus.OFFLINE)
        await manager.register_peer(peer)
        # Force status back to OFFLINE (register_peer sets ONLINE)
        peer.status = PeerStatus.OFFLINE

        await manager.lazy_repair()
        transport.is_connected.assert_not_called()

    async def test_no_transport_is_noop(self):
        """If no transport is provided, repair does nothing."""
        manager = _make_manager(transport=None)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        # Should not raise
        await manager.lazy_repair()


# -- concurrent lock --


class TestLazyRepairConcurrency:
    async def test_concurrent_lock_prevents_double_repair(self):
        """Second concurrent call is skipped when lock is held."""
        transport = MagicMock(spec=WebSocketTransport)
        manager = _make_manager(transport=transport)

        peer = _make_peer()
        await manager.register_peer(peer)

        transport.is_connected = MagicMock(return_value=True)

        # Ping takes 0.2s to simulate slow network
        async def slow_ping(*args, **kwargs):
            await asyncio.sleep(0.2)
            return {"type": "pong"}

        transport.ping = AsyncMock(side_effect=slow_ping)

        # Expire debounce so both calls attempt repair
        manager._last_repair = 0.0

        # Run two repairs concurrently
        await asyncio.gather(manager.lazy_repair(), manager.lazy_repair())

        # Only one should have actually run (the other sees the lock held)
        assert transport.is_connected.call_count == 1


# -- ping/pong transport --


class TestPingPong:
    async def test_ping_sends_and_waits_for_pong(self):
        transport = WebSocketTransport()
        ws = AsyncMock()
        await transport.connect("session-1", ws)

        async def resolve_pong():
            await asyncio.sleep(0.05)
            transport.resolve_pong("session-1", {"type": "pong", "ts": 123})

        asyncio.create_task(resolve_pong())
        result = await transport.ping("session-1", timeout=2.0)
        assert result == {"type": "pong", "ts": 123}

    async def test_ping_timeout(self):
        transport = WebSocketTransport()
        ws = AsyncMock()
        await transport.connect("session-1", ws)

        with pytest.raises(asyncio.TimeoutError):
            await transport.ping("session-1", timeout=0.05)

    async def test_resolve_pong_no_pending_is_noop(self):
        transport = WebSocketTransport()
        # Should not raise
        transport.resolve_pong("nonexistent", {"type": "pong"})
