"""Tests for lazy_repair, active_repair, get_peer_by_pane, and ping/pong liveness."""

from __future__ import annotations

import asyncio
import os
import subprocess
import time
from datetime import datetime, timedelta, timezone
from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.peer_registry import PeerRegistry, SessionMapping
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerStatus


def _make_peer(
    peer_id: str = "repow-dev-abc12345",
    display_name: str = "myproject",
    status: PeerStatus = PeerStatus.ONLINE,
    backend: AgentType = AgentType.CLAUDE_CODE,
    pane_id: str | None = None,
    circle: str = "dev",
    last_seen: datetime | None = None,
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
        last_seen=last_seen or datetime.now(timezone.utc),
    )


def _make_manager(
    transport: WebSocketTransport | None = None,
    query_tracker: MagicMock | None = None,
    ask_tracker: AskTracker | MagicMock | None = None,
    *,
    peer_reap_ttl_seconds: float | None = None,
    prune_max_age_hours: float | None = None,
    stale_busy_timeout_seconds: float | None = None,
) -> PeerRegistry:
    config = Config()
    if peer_reap_ttl_seconds is not None:
        config.daemon.peer_reap_ttl_seconds = peer_reap_ttl_seconds
    if prune_max_age_hours is not None:
        config.daemon.prune_max_age_hours = prune_max_age_hours
    if stale_busy_timeout_seconds is not None:
        config.daemon.stale_busy_timeout_seconds = stale_busy_timeout_seconds
    router = MagicMock()
    registry = PeerRegistry(
        config=config,
        message_router=router,
        query_tracker=query_tracker,
        transport=transport,
        ask_tracker=ask_tracker,
    )
    registry._events.clear()
    return registry


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
        manager = _make_manager()

        peer = _make_peer(status=PeerStatus.OFFLINE)
        await manager.register_peer(peer)
        # Force offline with old last_seen to make eviction observable
        p = await manager.get_peer(peer.peer_id)
        p.status = PeerStatus.OFFLINE
        p.last_seen = datetime.now(timezone.utc) - timedelta(hours=100)

        # First call runs eviction
        await manager.lazy_repair()
        assert await manager.get_peer(peer.peer_id) is None

        # Re-add and make stale again
        peer2 = _make_peer(peer_id="repow-dev-xyz99999")
        await manager.register_peer(peer2)
        p2 = await manager.get_peer(peer2.peer_id)
        p2.status = PeerStatus.OFFLINE
        p2.last_seen = datetime.now(timezone.utc) - timedelta(hours=100)

        # Second call within 30s is a no-op (peer2 not evicted)
        await manager.lazy_repair()
        assert await manager.get_peer(peer2.peer_id) is not None

    async def test_runs_after_debounce_expires(self):
        manager = _make_manager()

        peer = _make_peer(status=PeerStatus.OFFLINE)
        await manager.register_peer(peer)
        p = await manager.get_peer(peer.peer_id)
        p.status = PeerStatus.OFFLINE
        p.last_seen = datetime.now(timezone.utc) - timedelta(hours=100)

        await manager.lazy_repair()
        assert await manager.get_peer(peer.peer_id) is None

        # Re-add a stale peer
        peer2 = _make_peer(peer_id="repow-dev-xyz99999")
        await manager.register_peer(peer2)
        p2 = await manager.get_peer(peer2.peer_id)
        p2.status = PeerStatus.OFFLINE
        p2.last_seen = datetime.now(timezone.utc) - timedelta(hours=100)

        # Simulate 31s passing
        manager._last_repair = time.monotonic() - 31.0

        await manager.lazy_repair()
        assert await manager.get_peer(peer2.peer_id) is None

    async def test_connected_unsafe_peer_is_demoted_after_three_strikes(self):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": False})
        transport.disconnect = AsyncMock(return_value=True)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        # Two honest "pane gone" verdicts: still online (strikes 1 and 2).
        await manager._demote_unsafe_connected_peers()
        await manager._demote_unsafe_connected_peers()
        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE

        # Third strike demotes terminally.
        await manager._demote_unsafe_connected_peers()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE
        transport.ping.assert_awaited_with(peer.peer_id, timeout=1.0)
        # Terminal offline severs the socket, identity-checked against the
        # connection snapshotted under the lock.
        transport.disconnect.assert_awaited_once_with(
            peer.peer_id, transport.current_websocket.return_value,
        )
        events = manager.get_events()
        assert [event["type"] for event in events] == [
            "peer_contradiction",
            "peer_offline",
        ]
        offline = events[1]
        assert offline["peer_id"] == peer.peer_id
        assert offline["old_status"] == "online"
        assert offline["new_status"] == "offline"
        assert offline["reason"] == "pane_missing"
        assert offline["source"] == "lazy_repair"
        # Terminal demotion retires the identity.
        assert peer.peer_id in manager._retired

    async def test_pane_alive_true_resets_strikes(self):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.disconnect = AsyncMock(return_value=True)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": False})
        await manager._demote_unsafe_connected_peers()
        await manager._demote_unsafe_connected_peers()
        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": True})
        await manager._demote_unsafe_connected_peers()
        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": False})
        await manager._demote_unsafe_connected_peers()
        await manager._demote_unsafe_connected_peers()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE

    async def test_pane_alive_omitted_is_inconclusive(self):
        """Modern hooks omit pane_alive when tmux/ps checks failed transiently:
        no strike, no reset."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.disconnect = AsyncMock(return_value=True)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": False})
        await manager._demote_unsafe_connected_peers()
        await manager._demote_unsafe_connected_peers()
        # Inconclusive pong: strikes are preserved, not reset.
        transport.ping = AsyncMock(return_value={"type": "pong"})
        await manager._demote_unsafe_connected_peers()
        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        # One more honest false completes the three strikes.
        transport.ping = AsyncMock(return_value={"type": "pong", "pane_alive": False})
        await manager._demote_unsafe_connected_peers()
        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE

    async def test_connected_peer_ping_timeout_is_not_demoted(self):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(side_effect=asyncio.TimeoutError("no pong"))
        transport.disconnect = AsyncMock(return_value=True)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        transport.ping.assert_awaited_once_with(peer.peer_id, timeout=1.0)
        transport.disconnect.assert_not_awaited()
        qt.cancel_queries_to_peer.assert_not_called()

    async def test_disconnected_pane_peer_with_live_runtime_stays_live(self):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(
            pane_id="%5",
            status=PeerStatus.BUSY,
        )
        peer.agent_pid = os.getpid()
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.BUSY
            live.agent_pid = os.getpid()

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.BUSY
        qt.cancel_queries_to_peer.assert_not_called()

    async def test_disconnected_pane_peer_without_runtime_evidence_is_demoted(
        self, monkeypatch,
    ):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        peer.agent_pid = 999_999_999
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.agent_pid = 999_999_999

        monkeypatch.setattr(
            "repowire.daemon.registry_repair.os.kill",
            lambda _pid, _sig: (_ for _ in ()).throw(ProcessLookupError()),
        )
        monkeypatch.setattr(
            "repowire.daemon.registry_repair.subprocess.run",
            lambda *_args, **_kwargs: subprocess.CompletedProcess(
                args=["tmux"], returncode=1, stdout="", stderr="can't find pane",
            ),
        )

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE
        qt.cancel_queries_to_peer.assert_awaited_once_with(peer.peer_id)
        events = manager.get_events()
        assert [event["type"] for event in events] == [
            "peer_contradiction",
            "peer_contradiction",
            "peer_offline",
        ]
        offline = events[2]
        assert offline["peer_id"] == peer.peer_id
        assert offline["reason"] == "no_websocket_no_runtime_evidence"
        assert offline["source"] == "lazy_repair"
        assert offline["context"]["contradiction"] == "ONLINE_BUT_NO_WS"

    async def test_disconnected_pane_peer_with_dead_agent_pid_and_live_pane_is_demoted(
        self, monkeypatch,
    ):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        peer.agent_pid = 999_999_999
        await manager.register_peer(peer)
        async with manager._lock:
            manager._peers[peer.peer_id].agent_pid = 999_999_999

        monkeypatch.setattr(
            "repowire.daemon.registry_repair.os.kill",
            lambda _pid, _sig: (_ for _ in ()).throw(ProcessLookupError()),
        )
        monkeypatch.setattr(
            "repowire.daemon.registry_repair.subprocess.run",
            lambda *_args, **_kwargs: subprocess.CompletedProcess(
                args=["tmux"], returncode=0, stdout="123\n", stderr="",
            ),
        )

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE
        qt.cancel_queries_to_peer.assert_awaited_once_with(peer.peer_id)
        events = manager.get_events()
        assert [event["type"] for event in events] == [
            "peer_contradiction",
            "peer_contradiction",
            "peer_offline",
        ]
        assert events[2]["reason"] == "no_websocket_no_runtime_evidence"

    async def test_stale_busy_working_peer_repairs_to_idle(self):
        manager = _make_manager(stale_busy_timeout_seconds=60)
        old_seen = datetime.now(timezone.utc) - timedelta(seconds=61)
        peer = _make_peer(status=PeerStatus.BUSY, last_seen=old_seen)
        peer.turn_state = "working"
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.BUSY
            live.turn_state = "working"
            live.last_seen = old_seen

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        assert result.turn_state == "idle"

    async def test_recent_busy_working_peer_is_not_repaired(self):
        manager = _make_manager(stale_busy_timeout_seconds=60)
        recent_seen = datetime.now(timezone.utc) - timedelta(seconds=59)
        peer = _make_peer(status=PeerStatus.BUSY, last_seen=recent_seen)
        peer.turn_state = "working"
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.BUSY
            live.turn_state = "working"
            live.last_seen = recent_seen

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.BUSY
        assert result.turn_state == "working"

    async def test_busy_awaiting_input_is_not_repaired(self):
        manager = _make_manager(stale_busy_timeout_seconds=60)
        old_seen = datetime.now(timezone.utc) - timedelta(seconds=61)
        peer = _make_peer(status=PeerStatus.BUSY, last_seen=old_seen)
        peer.turn_state = "awaiting_input"
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.BUSY
            live.turn_state = "awaiting_input"
            live.last_seen = old_seen

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.BUSY
        assert result.turn_state == "awaiting_input"


class TestLazyRepairReaper:
    async def test_stale_disconnected_online_peer_demotes_then_reaps(self):
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        transport.disconnect = AsyncMock(return_value=True)
        ask_tracker = MagicMock()
        ask_tracker.forget_peer = AsyncMock(return_value=1)
        ask_tracker.snapshot_pending_replies_for_peer = AsyncMock(return_value=[])
        ask_tracker.snapshot_expired_pending_replies = AsyncMock(return_value=[])
        ask_tracker.evict_expired = AsyncMock(return_value=0)
        manager = _make_manager(
            transport=transport,
            ask_tracker=ask_tracker,
            peer_reap_ttl_seconds=60,
        )
        stale_seen = datetime.now(timezone.utc) - timedelta(
            seconds=manager.heartbeat_tolerance() + 1,
        )
        peer = _make_peer(
            status=PeerStatus.ONLINE,
            pane_id=None,
            last_seen=stale_seen,
        )
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.ONLINE
            live.last_seen = stale_seen

        await manager.lazy_repair()

        demoted = await manager.get_peer(peer.peer_id)
        assert demoted is not None
        assert demoted.status == PeerStatus.OFFLINE
        assert manager.get_events()[-1]["reason"] == "no_websocket_no_pane"

        async with manager._lock:
            manager._peers[peer.peer_id].last_seen = (
                datetime.now(timezone.utc) - timedelta(seconds=61)
            )
        manager._last_repair = time.monotonic() - 31.0

        await manager.lazy_repair()

        assert await manager.get_peer(peer.peer_id) is None
        transport.disconnect.assert_awaited_once_with(peer.peer_id)
        ask_tracker.forget_peer.assert_awaited_once_with(peer.peer_id)

    async def test_spares_offline_peer_after_ttl_with_runtime_evidence(
        self, monkeypatch,
    ):
        transport = MagicMock(spec=WebSocketTransport)
        transport.disconnect = AsyncMock(return_value=True)
        ask_tracker = MagicMock()
        ask_tracker.forget_peer = AsyncMock(return_value=1)
        ask_tracker.snapshot_pending_replies_for_peer = AsyncMock(return_value=[])
        ask_tracker.snapshot_expired_pending_replies = AsyncMock(return_value=[])
        ask_tracker.evict_expired = AsyncMock(return_value=0)
        manager = _make_manager(
            transport=transport,
            ask_tracker=ask_tracker,
            peer_reap_ttl_seconds=600,
        )
        stale_seen = datetime.now(timezone.utc) - timedelta(seconds=601)
        peer = _make_peer(
            status=PeerStatus.OFFLINE,
            pane_id="%5",
            last_seen=stale_seen,
        )
        peer.agent_pid = 12345
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = stale_seen
            live.agent_pid = 12345
            manager._mappings[peer.peer_id] = SessionMapping(
                session_id=peer.peer_id,
                display_name=peer.display_name,
                circle=peer.circle,
                backend=peer.backend,
                path=peer.path,
            )

        monkeypatch.setattr(
            "repowire.daemon.peer_registry.has_runtime_evidence",
            lambda _peer: True,
        )

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result is not None
        assert result.status == PeerStatus.OFFLINE
        assert manager._mappings[peer.peer_id].display_name == peer.display_name
        transport.disconnect.assert_not_awaited()
        ask_tracker.forget_peer.assert_not_awaited()
        events = manager.get_events()
        assert len(events) == 1
        assert events[0]["type"] == "offline_peer_still_has_runtime_evidence"
        assert events[0]["peer_id"] == peer.peer_id
        assert events[0]["pane_id"] == "%5"
        assert events[0]["agent_pid"] == 12345
        assert events[0]["reason"] == "offline_ttl_with_runtime_evidence"

    async def test_spared_offline_peer_keeps_mapping_and_stashed_ask(
        self, monkeypatch,
    ):
        transport = MagicMock(spec=WebSocketTransport)
        transport.disconnect = AsyncMock(return_value=True)
        ask_tracker = AskTracker()
        manager = _make_manager(
            transport=transport,
            ask_tracker=ask_tracker,
            peer_reap_ttl_seconds=600,
        )
        stale_seen = datetime.now(timezone.utc) - timedelta(seconds=601)
        peer = _make_peer(
            status=PeerStatus.OFFLINE,
            pane_id="%5",
            last_seen=stale_seen,
        )
        peer.agent_pid = 12345
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = stale_seen
            live.agent_pid = 12345
            manager._mappings[peer.peer_id] = SessionMapping(
                session_id=peer.peer_id,
                display_name=peer.display_name,
                circle=peer.circle,
                backend=peer.backend,
                path=peer.path,
            )
        cid = await ask_tracker.register(
            from_peer_id=peer.peer_id,
            from_peer_name=peer.display_name,
            to_peer_id="repow-dev-answerer",
            to_peer_name="answerer",
            text="question",
        )
        assert await ask_tracker.set_pending_reply(cid, "stashed reply")

        monkeypatch.setattr(
            "repowire.daemon.peer_registry.has_runtime_evidence",
            lambda _peer: True,
        )

        await manager.lazy_repair()

        assert await manager.get_peer(peer.peer_id) is not None
        assert peer.peer_id in manager._mappings
        ask = await ask_tracker.get(cid)
        assert ask is not None
        assert ask.pending_reply == "stashed reply"
        assert "pending_reply_lost" not in {
            event["type"] for event in manager.get_events()
        }
        transport.disconnect.assert_not_awaited()

    async def test_reaps_offline_peer_after_ttl(self, monkeypatch):
        transport = MagicMock(spec=WebSocketTransport)
        transport.disconnect = AsyncMock(return_value=True)
        ask_tracker = MagicMock()
        ask_tracker.forget_peer = AsyncMock(return_value=1)
        ask_tracker.snapshot_pending_replies_for_peer = AsyncMock(return_value=[])
        ask_tracker.snapshot_expired_pending_replies = AsyncMock(return_value=[])
        ask_tracker.evict_expired = AsyncMock(return_value=0)
        manager = _make_manager(
            transport=transport,
            ask_tracker=ask_tracker,
            peer_reap_ttl_seconds=600,
        )
        stale_seen = datetime.now(timezone.utc) - timedelta(seconds=601)
        peer = _make_peer(
            status=PeerStatus.OFFLINE,
            pane_id="%5",
            last_seen=stale_seen,
        )
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = stale_seen
            manager._mappings[peer.peer_id] = SessionMapping(
                session_id=peer.peer_id,
                display_name=peer.display_name,
                circle=peer.circle,
                backend=peer.backend,
                path=peer.path,
            )

        monkeypatch.setattr(
            "repowire.daemon.peer_registry.has_runtime_evidence",
            lambda _peer: False,
        )

        await manager.lazy_repair()

        assert await manager.get_peer(peer.peer_id) is None
        assert peer.peer_id not in manager._mappings
        transport.disconnect.assert_awaited_once_with(peer.peer_id)
        ask_tracker.forget_peer.assert_awaited_once_with(peer.peer_id)
        events = manager.get_events()
        assert len(events) == 1
        assert events[0]["type"] == "peer_reaped"
        assert events[0]["peer_id"] == peer.peer_id
        assert events[0]["display_name"] == peer.display_name
        assert events[0]["backend"] == peer.backend.value
        assert events[0]["pane_id"] == "%5"
        assert events[0]["reason"] == "offline_ttl"

    async def test_stale_evict_spares_offline_peer_with_runtime_evidence(
        self, monkeypatch,
    ):
        ask_tracker = MagicMock()
        ask_tracker.forget_peer = AsyncMock(return_value=1)
        ask_tracker.snapshot_pending_replies_for_peer = AsyncMock(return_value=[])
        ask_tracker.snapshot_expired_pending_replies = AsyncMock(return_value=[])
        ask_tracker.evict_expired = AsyncMock(return_value=0)
        manager = _make_manager(
            ask_tracker=ask_tracker,
            peer_reap_ttl_seconds=0,
            prune_max_age_hours=1 / 3600,
        )
        stale_seen = datetime.now(timezone.utc) - timedelta(seconds=2)
        peer = _make_peer(
            status=PeerStatus.OFFLINE,
            pane_id="%5",
            last_seen=stale_seen,
        )
        peer.agent_pid = 12345
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = stale_seen
            live.agent_pid = 12345
            manager._mappings[peer.peer_id] = SessionMapping(
                session_id=peer.peer_id,
                display_name=peer.display_name,
                circle=peer.circle,
                backend=peer.backend,
                path=peer.path,
            )

        monkeypatch.setattr(
            "repowire.daemon.peer_registry.has_runtime_evidence",
            lambda _peer: True,
        )

        await manager.lazy_repair()

        assert await manager.get_peer(peer.peer_id) is not None
        assert peer.peer_id in manager._mappings
        ask_tracker.forget_peer.assert_not_awaited()
        events = manager.get_events()
        assert len(events) == 1
        assert events[0]["type"] == "offline_peer_still_has_runtime_evidence"
        assert events[0]["peer_id"] == peer.peer_id
        assert events[0]["reason"] == "stale_evict_with_runtime_evidence"

    async def test_does_not_reap_offline_peer_younger_than_ttl(self):
        manager = _make_manager(peer_reap_ttl_seconds=600)
        recent_seen = datetime.now(timezone.utc) - timedelta(seconds=599)
        peer = _make_peer(status=PeerStatus.OFFLINE, last_seen=recent_seen)
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = recent_seen

        await manager.lazy_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result is not None
        assert result.status == PeerStatus.OFFLINE

    async def test_reaper_disabled_with_zero_ttl(self):
        manager = _make_manager(peer_reap_ttl_seconds=0)
        stale_seen = datetime.now(timezone.utc) - timedelta(hours=1)
        peer = _make_peer(status=PeerStatus.OFFLINE, last_seen=stale_seen)
        await manager.register_peer(peer)
        async with manager._lock:
            live = manager._peers[peer.peer_id]
            live.status = PeerStatus.OFFLINE
            live.last_seen = stale_seen

        await manager.lazy_repair()

        assert await manager.get_peer(peer.peer_id) is not None
        assert manager.get_events() == []


# -- active_repair liveness checks --


class TestActiveRepairLiveness:
    async def test_no_ws_marks_offline(self):
        """Peer with no WebSocket or runtime evidence is marked OFFLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.active_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE
        events = manager.get_events()
        assert len(events) == 1
        assert events[0]["type"] == "peer_offline"
        assert events[0]["peer_id"] == peer.peer_id
        assert events[0]["reason"] == "active_repair_no_pong"
        assert events[0]["source"] == "active_repair"

    async def test_no_ws_with_live_pane_runtime_stays_online(self):
        """Active repair separates inbound transport loss from runtime liveness."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        transport.ping = AsyncMock(side_effect=AssertionError("should not ping without WS"))
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        peer.agent_pid = os.getpid()
        await manager.register_peer(peer)
        async with manager._lock:
            manager._peers[peer.peer_id].agent_pid = os.getpid()

        await manager.active_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        transport.ping.assert_not_awaited()
        qt.cancel_queries_to_peer.assert_not_called()

    async def test_pong_alive_stays_online(self):
        """Peer that responds to ping stays ONLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(return_value={"type": "pong"})
        manager = _make_manager(transport=transport)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.active_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE

    async def test_ping_timeout_marks_offline(self):
        """Peer that doesn't respond to ping is marked OFFLINE."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=True)
        transport.ping = AsyncMock(side_effect=TimeoutError("no pong"))
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        await manager.active_repair()

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

        await manager.active_repair()

        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
        transport.ping.assert_not_awaited()

    async def test_offline_peers_skipped(self):
        """OFFLINE peers are not checked during active repair."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(side_effect=AssertionError("should not be called"))
        manager = _make_manager(transport=transport)

        peer = _make_peer(status=PeerStatus.OFFLINE)
        await manager.register_peer(peer)
        # Force status back to OFFLINE (register_peer sets ONLINE)
        peer.status = PeerStatus.OFFLINE

        await manager.active_repair()
        transport.is_connected.assert_not_called()

    async def test_no_transport_is_noop(self):
        """If no transport is provided, active repair does nothing."""
        manager = _make_manager(transport=None)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        # Should not raise
        await manager.active_repair()

    async def test_stale_offline_evicted(self):
        """Stale OFFLINE peers are evicted during active repair."""
        transport = MagicMock(spec=WebSocketTransport)
        transport.is_connected = MagicMock(return_value=False)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(status=PeerStatus.ONLINE)
        await manager.register_peer(peer)

        # First repair marks peer offline (no WS connection)
        await manager.active_repair()
        result = await manager.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE

        # Make stale
        result.last_seen = datetime.now(timezone.utc) - timedelta(hours=100)

        # Second repair evicts stale peer
        await manager.active_repair()
        assert await manager.get_peer(peer.peer_id) is None


# -- concurrent lock --


class TestActiveRepairConcurrency:
    async def test_concurrent_lock_serializes_repair(self):
        """Concurrent active_repair calls are serialized, not skipped."""
        transport = MagicMock(spec=WebSocketTransport)
        manager = _make_manager(transport=transport)

        peer = _make_peer()
        await manager.register_peer(peer)

        transport.is_connected = MagicMock(return_value=True)

        async def slow_ping(*args, **kwargs):
            await asyncio.sleep(0.05)
            return {"type": "pong"}

        transport.ping = AsyncMock(side_effect=slow_ping)

        # Both calls run (second waits for first to release lock)
        await asyncio.gather(
            manager.active_repair(), manager.active_repair(),
        )

        # Both actually ran (serialized, not skipped)
        assert transport.is_connected.call_count == 2


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


class TestRetirement:
    async def test_terminal_offline_on_evicted_id_still_retires(self):
        """A terminal offline for an id no longer in the registry must record
        retirement so the orphan it came from cannot re-register via mapping."""
        manager = _make_manager()
        await manager.mark_offline(
            "repow-dev-gone1234", reason="agent_exited", source="ws_hook",
            terminal=True,
        )
        assert "repow-dev-gone1234" in manager._retired

    async def test_terminal_offline_on_unknown_display_name_does_not_retire(self):
        manager = _make_manager()
        await manager.mark_offline(
            "some-display-name", reason="agent_exited", source="ws_hook",
            terminal=True,
        )
        assert "some-display-name" not in manager._retired

    async def test_retirement_survives_restart_via_state_db(self, tmp_path):
        """A daemon restart must not hand an orphan hook one free
        re-registration: retirement is persisted and reloaded."""
        from repowire.daemon.state.database import StateDatabase

        db = StateDatabase(tmp_path / "state.db")
        manager = _make_manager()
        manager._state_db = db
        await manager.mark_offline(
            "repow-dev-zombie99", reason="agent_exited", source="ws_hook",
            terminal=True,
        )
        assert "repow-dev-zombie99" in manager._retired

        # Fresh registry over the same DB (simulated restart) reloads it.
        reborn = _make_manager()
        reborn._state_db = db
        reborn._load_retired()
        assert "repow-dev-zombie99" in reborn._retired

        # Unretire clears it durably.
        reborn._unretire("repow-dev-zombie99")
        third = _make_manager()
        third._state_db = db
        third._load_retired()
        assert "repow-dev-zombie99" not in third._retired

    async def test_terminal_offline_closes_the_doomed_websocket(self):
        """Popping the transport dict alone leaves the orphan hook's TCP
        connection open — it never reconnects, never hits the retirement
        guard, and holds its pane flock forever. Terminal offline must
        actually close the socket."""
        doomed_ws = MagicMock()
        doomed_ws.close = AsyncMock()
        transport = MagicMock(spec=WebSocketTransport)
        transport.current_websocket = MagicMock(return_value=doomed_ws)
        transport.disconnect = AsyncMock(return_value=True)
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)
        await manager.mark_offline(
            peer.peer_id, reason="agent_exited", source="ws_hook", terminal=True,
        )

        transport.disconnect.assert_awaited_once_with(peer.peer_id, doomed_ws)
        doomed_ws.close.assert_awaited_once()

    async def test_terminal_offline_skips_close_when_socket_was_replaced(self):
        doomed_ws = MagicMock()
        doomed_ws.close = AsyncMock()
        transport = MagicMock(spec=WebSocketTransport)
        transport.current_websocket = MagicMock(return_value=doomed_ws)
        transport.disconnect = AsyncMock(return_value=False)  # already replaced
        qt = MagicMock()
        qt.cancel_queries_to_peer = AsyncMock(return_value=0)
        manager = _make_manager(transport=transport, query_tracker=qt)

        peer = _make_peer(pane_id="%5", status=PeerStatus.ONLINE)
        await manager.register_peer(peer)
        await manager.mark_offline(
            peer.peer_id, reason="agent_exited", source="ws_hook", terminal=True,
        )
        doomed_ws.close.assert_not_awaited()
