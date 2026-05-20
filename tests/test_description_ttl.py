"""Tests for peer description TTL (clear-on-read).

Issue #162: peer descriptions never auto-clear after a task completes.
Daemon-side TTL with clear-on-read semantics — no background sweep, so the
cleared state surfaces on the very next list_peers / get_peer call.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import AgentType, Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.peers import Peer, PeerStatus


@pytest.fixture
def mock_router():
    router = MagicMock(spec=MessageRouter)
    router.send_query = AsyncMock(return_value="ok")
    router.send_notification = AsyncMock()
    router.broadcast = AsyncMock(return_value=[])
    return router


def _make_config(ttl_seconds: float) -> Config:
    cfg = Config()
    cfg.daemon.description_ttl_seconds = ttl_seconds
    return cfg


def _make_peer(name: str = "dev") -> Peer:
    return Peer(
        peer_id=f"repow-test-{name}",
        display_name=name,
        path="/app",
        machine="laptop",
        status=PeerStatus.ONLINE,
    )


@pytest.mark.asyncio
async def test_description_set_then_read_within_ttl_preserved(mock_router):
    """Set description → read within TTL → description survives."""
    registry = PeerRegistry(config=_make_config(60), message_router=mock_router)
    await registry.register_peer(_make_peer())

    await registry.update_description("dev", "fixing auth bug")

    peer = await registry.get_peer("dev")
    assert peer is not None
    assert peer.description == "fixing auth bug"

    all_peers = await registry.get_all_peers()
    assert all_peers[0].description == "fixing auth bug"


@pytest.mark.asyncio
async def test_description_clears_on_read_after_ttl(mock_router):
    """Set description → wind set_at past TTL → next read returns empty."""
    registry = PeerRegistry(config=_make_config(60), message_router=mock_router)
    await registry.register_peer(_make_peer())

    await registry.update_description("dev", "working on #162")

    # Wind the internal set_at back beyond the TTL window so the next read
    # trips clear-on-read without sleeping.
    peer_id = "repow-test-dev"
    registry._description_set_at[peer_id] = (
        datetime.now(timezone.utc) - timedelta(seconds=120)
    )

    peer = await registry.get_peer("dev")
    assert peer is not None
    assert peer.description == ""
    # Internal timestamp store is cleaned up so we don't leak entries.
    assert peer_id not in registry._description_set_at

    # list_peers / get_all_peers reflects the cleared state immediately —
    # no sweep needed.
    all_peers = await registry.get_all_peers()
    assert all_peers[0].description == ""


@pytest.mark.asyncio
async def test_ttl_zero_disables_clearing(mock_router):
    """description_ttl_seconds=0 disables the TTL entirely."""
    registry = PeerRegistry(config=_make_config(0), message_router=mock_router)
    await registry.register_peer(_make_peer())

    await registry.update_description("dev", "long-running task")

    # Even a wildly old set_at must not trigger clearing when TTL is 0.
    registry._description_set_at["repow-test-dev"] = (
        datetime.now(timezone.utc) - timedelta(hours=24)
    )

    peer = await registry.get_peer("dev")
    assert peer is not None
    assert peer.description == "long-running task"


@pytest.mark.asyncio
async def test_update_description_resets_ttl_window(mock_router):
    """Updating the description re-stamps set_at, sliding the TTL window."""
    registry = PeerRegistry(config=_make_config(60), message_router=mock_router)
    await registry.register_peer(_make_peer())

    await registry.update_description("dev", "task A")
    # Age it past the TTL.
    registry._description_set_at["repow-test-dev"] = (
        datetime.now(timezone.utc) - timedelta(seconds=120)
    )
    # New description should win, with a fresh set_at, and survive read.
    await registry.update_description("dev", "task B")

    peer = await registry.get_peer("dev")
    assert peer is not None
    assert peer.description == "task B"


@pytest.mark.asyncio
async def test_clearing_description_drops_set_at(mock_router):
    """Setting the description to '' (explicit clear) removes the timestamp."""
    registry = PeerRegistry(config=_make_config(60), message_router=mock_router)
    await registry.register_peer(_make_peer())

    await registry.update_description("dev", "task")
    assert "repow-test-dev" in registry._description_set_at

    await registry.update_description("dev", "")
    assert "repow-test-dev" not in registry._description_set_at


@pytest.mark.asyncio
async def test_restored_description_gets_bounded_ttl_window(tmp_path, mock_router):
    """Descriptions restored from sessions.json are stamped on first read.

    The first read preserves the restored description because the daemon cannot
    know when it was set, but that read starts a bounded TTL window so the stale
    task state clears on a later read instead of living forever.
    """
    path = tmp_path / "sessions.json"
    restored_dir = tmp_path / "restored-desc"
    restored_dir.mkdir()
    cfg = _make_config(60)
    registry = PeerRegistry(config=cfg, message_router=mock_router, persistence_path=path)
    peer_id, _ = await registry.allocate_and_register(
        circle="team-a",
        backend=AgentType.CLAUDE_CODE,
        path=str(restored_dir),
    )
    await registry.update_description(peer_id, "old task")
    registry._persist_mappings()

    restarted = PeerRegistry(config=cfg, message_router=mock_router, persistence_path=path)
    new_id, _ = await restarted.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(restored_dir),
    )
    assert new_id == peer_id

    first_read = await restarted.get_peer(peer_id)
    assert first_read is not None
    assert first_read.description == "old task"
    assert peer_id in restarted._description_set_at

    restarted._description_set_at[peer_id] = (
        datetime.now(timezone.utc) - timedelta(seconds=120)
    )
    second_read = await restarted.get_peer(peer_id)
    assert second_read is not None
    assert second_read.description == ""
