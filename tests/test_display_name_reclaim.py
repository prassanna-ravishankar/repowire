"""display_name reclaim across reconnect/restart (repowire-pql).

Two peers cannot share a runtime session id, so a registration carrying the
same session id as a name-blocking peer IS that peer reconnecting and must
reclaim its name instead of churning a -2/-3 suffix — even if the daemon has
not yet demoted the prior instance to OFFLINE (the case after a daemon restart
re-registers the whole mesh).
"""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest

from repowire.config.models import AgentType, Config
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.peers import PeerStatus


@pytest.fixture
def registry() -> PeerRegistry:
    return PeerRegistry(config=Config(), message_router=AsyncMock())


async def _register(reg: PeerRegistry, *, session_id: str, status=PeerStatus.ONLINE):
    return await reg.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/proj",
        metadata={"runtime_session_id": session_id},
        initial_status=status,
    )


@pytest.mark.asyncio
async def test_same_session_reconnect_reclaims_name_without_suffix(registry):
    # First registration claims "proj-claude-code".
    _id1, name1 = await _register(registry, session_id="sess-A")
    assert name1 == "proj-claude-code"

    # The prior instance is still ONLINE (daemon hasn't demoted it yet), but the
    # reconnect carries the SAME runtime session id -> it is the same logical
    # peer and must reclaim the name, not get suffixed.
    assert registry._peers[_id1].status == PeerStatus.ONLINE
    _id2, name2 = await _register(registry, session_id="sess-A")
    assert name2 == "proj-claude-code", "same-session reconnect should reclaim its name"
    # The stale prior instance is gone (reclaimed).
    assert _id1 not in registry._peers


@pytest.mark.asyncio
async def test_same_session_reconnect_reclaims_from_busy_blocker(registry):
    # BUSY is a common mid-turn state; a same-session reconnect must reclaim the
    # name from a BUSY ghost too, not just ONLINE (codex review).
    _id1, name1 = await _register(registry, session_id="sess-A")
    registry._peers[_id1].status = PeerStatus.BUSY
    _id2, name2 = await _register(registry, session_id="sess-A")
    assert name1 == "proj-claude-code"
    assert name2 == "proj-claude-code"
    assert _id1 not in registry._peers


@pytest.mark.asyncio
async def test_distinct_session_same_path_gets_suffixed(registry):
    # Two genuinely distinct live peers on the same path+backend+circle must NOT
    # collide -- the second keeps a -2 suffix.
    _id1, name1 = await _register(registry, session_id="sess-A")
    _id2, name2 = await _register(registry, session_id="sess-B")
    assert name1 == "proj-claude-code"
    assert name2 == "proj-2-claude-code"
    # Both remain live and addressable.
    assert _id1 in registry._peers and _id2 in registry._peers


@pytest.mark.asyncio
async def test_offline_prior_instance_is_still_reclaimed(registry):
    # The original clean-takeover rule still holds: an OFFLINE blocker is pruned
    # for name reclaim even when the new registration has a different session.
    _id1, _name1 = await _register(registry, session_id="sess-A")
    registry._peers[_id1].status = PeerStatus.OFFLINE
    _id2, name2 = await _register(registry, session_id="sess-B")
    assert name2 == "proj-claude-code"
    assert _id1 not in registry._peers


@pytest.mark.asyncio
async def test_no_session_id_falls_back_to_suffix(registry):
    # Without a runtime session id we cannot prove "same peer", so a live blocker
    # still gets a suffix (no false reclaim).
    _id1, name1 = await registry.allocate_and_register(
        circle="default", backend=AgentType.CLAUDE_CODE, path="/tmp/proj",
    )
    _id2, name2 = await registry.allocate_and_register(
        circle="default", backend=AgentType.CLAUDE_CODE, path="/tmp/proj",
    )
    assert name1 == "proj-claude-code"
    assert name2 == "proj-2-claude-code"
