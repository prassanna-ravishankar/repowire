"""Tests for MCP server self-identity resolution.

Covers #107: when two peers share (cwd, backend), MCP must resolve its
own from_peer name correctly. The fix relies on (a) ppid-chain pane
discovery in hooks._tmux and (b) reading pane runtime metadata when the
daemon /peers/by-pane lookup misses.
"""

from __future__ import annotations

from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from repowire.mcp import server as mcp_server


@pytest.fixture(autouse=True)
def reset_cache():
    mcp_server._cached_peer_name = None
    yield
    mcp_server._cached_peer_name = None


def _matching_meta(extra: dict | None = None) -> dict:
    """Build a metadata dict whose cwd+backend match the current process."""
    base = {
        "display_name": "proj-2-claude-code",
        "peer_id": "p-2",
        "cwd": str(Path.cwd()),
        "backend": mcp_server._detect_backend(),
    }
    if extra:
        base.update(extra)
    return base


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
             return_value=_matching_meta(),
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2-claude-code"


@pytest.mark.asyncio
async def test_rejects_stale_metadata_with_cwd_mismatch():
    """Pane metadata can outlive the session that wrote it (daemon restart
    + pane reuse). Reject metadata pointing at a different cwd — fall
    through to cwd-folder fallback rather than mis-claiming identity."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    stale = _matching_meta({"cwd": "/some/other/abandoned/path"})

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=stale), \
         patch.object(mcp_server, "get_display_name", return_value="current-folder"):
        name = await mcp_server._get_my_peer_name()
    assert name == "current-folder"


@pytest.mark.asyncio
async def test_rejects_stale_metadata_with_backend_mismatch():
    """Backend mismatch (e.g. codex peer reused a pane previously held by
    claude-code) also disqualifies the metadata."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    stale = _matching_meta({"backend": "some-other-backend"})

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=stale), \
         patch.object(mcp_server, "get_display_name", return_value="current-folder"):
        name = await mcp_server._get_my_peer_name()
    assert name == "current-folder"


@pytest.mark.asyncio
async def test_rejects_metadata_missing_cwd_or_backend():
    """Metadata without cwd+backend fields can't be validated — reject it."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(
             mcp_server,
             "read_pane_runtime_metadata",
             return_value={"display_name": "ghost", "peer_id": "g"},
         ), \
         patch.object(mcp_server, "get_display_name", return_value="current-folder"):
        name = await mcp_server._get_my_peer_name()
    assert name == "current-folder"


@pytest.mark.asyncio
async def test_cwd_fallback_not_cached():
    """If we fell through to cwd-folder, the next call should re-attempt
    daemon/metadata resolution — caching cwd-folder forever would lock in
    a wrong (un-suffixed) name even after the daemon is back."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    with patch.object(mcp_server, "get_pane_id", return_value=None), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(mcp_server, "get_display_name", return_value="fallback"):
        await mcp_server._get_my_peer_name()
    assert mcp_server._cached_peer_name is None

    # Daemon comes back; second call resolves to suffixed name via daemon
    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(
             mcp_server,
             "daemon_request",
             new=AsyncMock(return_value={"display_name": "proj-2", "peer_id": "p"}),
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2"


@pytest.mark.asyncio
async def test_two_peers_same_cwd_resolve_distinctly():
    """Smoke: simulate peer 1 and peer 2 both in the same cwd but with
    different pane_ids. With the fix, each resolves to its own display_name
    via the metadata file (not the un-suffixed cwd folder name)."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("registration race")

    pane_to_meta = {
        "%10": _matching_meta({"display_name": "proj-claude-code", "peer_id": "p-1"}),
        "%20": _matching_meta({"display_name": "proj-2-claude-code", "peer_id": "p-2"}),
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


def test_all_outbound_tools_strict_register():
    """Every MCP tool that sends from_peer to the daemon must gate on
    _ensure_registered(strict=True). Without this, the first MCP call
    after a hook drop can race the registration and emit fallback
    (cwd-folder) identity. kill_peer was missing this gate pre-#108 —
    this audit prevents the regression class from coming back.
    """
    import inspect

    from repowire.mcp import server as mod

    source = inspect.getsource(mod)
    # Outbound mesh tools that put from_peer / mutate other peers
    outbound_tools = ["ask", "ack", "notify_peer", "broadcast", "kill_peer"]
    # Locate each tool def + the next 30 lines, check for strict register
    for tool in outbound_tools:
        idx = source.find(f"async def {tool}(")
        assert idx >= 0, f"Could not find {tool} in mcp/server.py"
        body = source[idx : idx + 1500]
        assert "_ensure_registered(strict=True)" in body, (
            f"MCP tool {tool} must call _ensure_registered(strict=True) "
            f"before sending; otherwise from_peer can race a hook drop. "
            f"See PR #108 / Issue #107."
        )


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
