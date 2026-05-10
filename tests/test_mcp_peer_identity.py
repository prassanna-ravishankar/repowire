"""Tests for MCP server self-identity resolution.

Covers #107: when two peers share (cwd, backend), MCP must resolve its
own from_peer name correctly. The fix relies on (a) ppid-chain pane
discovery in hooks._tmux and (b) reading pane runtime metadata when the
daemon /peers/by-pane lookup misses.
"""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from repowire.mcp import server as mcp_server


@pytest.fixture(autouse=True)
def reset_cache():
    mcp_server._cached_peer_name = None
    yield
    mcp_server._cached_peer_name = None


@pytest.mark.asyncio
async def test_resolves_via_daemon_by_pane():
    """Primary path: daemon /peers/by-pane returns the display_name."""
    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(
             mcp_server, "daemon_request", new=AsyncMock(
                 return_value={"display_name": "proj-2-claude-code", "peer_id": "p-2"}
             )
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2-claude-code"


@pytest.mark.asyncio
async def test_falls_back_to_pane_metadata_when_daemon_misses():
    """Secondary path: when /peers/by-pane fails, read the on-disk metadata
    written by SessionStart. This is the #107 fix — without it, two peers
    sharing (cwd, backend) collide on the cwd-folder-name fallback."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("daemon unreachable / pane not registered yet")

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(
             mcp_server,
             "read_pane_runtime_metadata",
             return_value={"display_name": "proj-2-claude-code", "peer_id": "p-2"},
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2-claude-code"


@pytest.mark.asyncio
async def test_two_peers_same_cwd_resolve_distinctly():
    """Smoke: simulate peer 1 and peer 2 both in the same cwd but with
    different pane_ids. With the fix, each resolves to its own display_name
    via the metadata file (not the un-suffixed cwd folder name)."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("registration race")

    pane_to_meta = {
        "%10": {"display_name": "proj-claude-code", "peer_id": "p-1"},
        "%20": {"display_name": "proj-2-claude-code", "peer_id": "p-2"},
    }

    async def resolve_for_pane(pane_id: str) -> str:
        mcp_server._cached_peer_name = None
        with patch.object(mcp_server, "get_pane_id", return_value=pane_id), \
             patch.object(
                 mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)
             ), \
             patch.object(
                 mcp_server,
                 "read_pane_runtime_metadata",
                 side_effect=lambda pid: pane_to_meta.get(pid, {}),
             ):
            return await mcp_server._get_my_peer_name()

    name_peer1 = await resolve_for_pane("%10")
    name_peer2 = await resolve_for_pane("%20")
    assert name_peer1 == "proj-claude-code"
    assert name_peer2 == "proj-2-claude-code"
    assert name_peer1 != name_peer2


@pytest.mark.asyncio
async def test_falls_back_to_get_display_name_when_no_metadata():
    """Last resort: no pane, no metadata — use env/cwd-folder. Pre-#107
    behavior preserved for the single-peer-per-cwd case."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    with patch.object(mcp_server, "get_pane_id", return_value=None), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(mcp_server, "get_display_name", return_value="cwd-folder"):
        name = await mcp_server._get_my_peer_name()
    assert name == "cwd-folder"


@pytest.mark.asyncio
async def test_caches_after_first_resolution():
    with patch.object(mcp_server, "get_pane_id", return_value="%42") as mock_pane, \
         patch.object(
             mcp_server,
             "daemon_request",
             new=AsyncMock(return_value={"display_name": "p", "peer_id": "x"}),
         ):
        await mcp_server._get_my_peer_name()
        await mcp_server._get_my_peer_name()
    # Second call short-circuits via cache
    assert mock_pane.call_count == 1
