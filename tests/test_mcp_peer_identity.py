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
    mcp_server._cached_peer_id = None
    mcp_server._registered = False
    yield
    mcp_server._cached_peer_name = None
    mcp_server._cached_peer_id = None
    mcp_server._registered = False


def _matching_meta(extra: dict | None = None) -> dict:
    """Build pane metadata owned by the current mocked agent process."""
    base = {
        "display_name": "proj-2-claude-code",
        "peer_id": "p-2",
        "cwd": str(Path.cwd()),
        "backend": mcp_server._detect_backend(),
        "agent_pid": 12345,
    }
    if extra:
        base.update(extra)
    return base


def _matching_peer(extra: dict | None = None) -> dict:
    """Build a daemon peer dict whose peer_id matches _matching_meta()."""
    base = {
        "display_name": "proj-2-claude-code",
        "peer_id": "p-2",
        "path": str(Path.cwd()),
        "backend": mcp_server._detect_backend(),
    }
    if extra:
        base.update(extra)
    return base


@pytest.mark.asyncio
async def test_resolves_via_daemon_by_pane():
    """Primary path: daemon /peers/by-pane returns the display_name."""
    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=_matching_meta()), \
         patch.object(
             mcp_server, "daemon_request", new=AsyncMock(
                 return_value=_matching_peer()
             )
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2-claude-code"


@pytest.mark.asyncio
async def test_rejects_daemon_by_pane_backend_mismatch():
    """A temp same-pane process must not cache the incumbent's daemon identity."""
    incumbent = _matching_peer({
        "display_name": "orchestrator-codex",
        "peer_id": "repow-orch",
        "backend": "codex",
    })

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server, "_detect_backend", return_value="claude-code"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(return_value=incumbent)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value={
             "display_name": "orchestrator-codex",
             "peer_id": "repow-orch",
             "backend": "codex",
             "agent_pid": 77777,
         }), \
         patch.object(mcp_server, "get_display_name", return_value="temp-claude-code"):
        name = await mcp_server._get_my_peer_name()

    assert name == "temp-claude-code"
    assert mcp_server._cached_peer_id is None


@pytest.mark.asyncio
async def test_falls_back_to_pane_metadata_when_daemon_misses():
    """Secondary path: when /peers/by-pane fails, read the on-disk metadata
    written by SessionStart. This is the #107 fix — without it, two peers
    sharing (cwd, backend) collide on the cwd-folder-name fallback."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("daemon unreachable / pane not registered yet")

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(
             mcp_server,
             "read_pane_runtime_metadata",
             return_value=_matching_meta(),
         ):
        name = await mcp_server._get_my_peer_name()
    assert name == "proj-2-claude-code"


@pytest.mark.asyncio
async def test_rejects_stale_metadata_with_agent_pid_mismatch():
    """Reject metadata owned by a different agent process."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    stale = _matching_meta({"agent_pid": 77777})

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
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
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_miss)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=stale), \
         patch.object(mcp_server, "get_display_name", return_value="current-folder"):
        name = await mcp_server._get_my_peer_name()
    assert name == "current-folder"


@pytest.mark.asyncio
async def test_rejects_metadata_missing_backend_or_agent_pid():
    """Metadata without backend+agent_pid fields can't be validated."""
    async def daemon_miss(*_args, **_kw):
        raise RuntimeError("nope")

    with patch.object(mcp_server, "get_pane_id", return_value="%42"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
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
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=_matching_meta({
             "display_name": "proj-2",
             "peer_id": "p",
         })), \
         patch.object(
             mcp_server,
             "daemon_request",
             new=AsyncMock(return_value=_matching_peer({"display_name": "proj-2", "peer_id": "p"})),
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
             patch.object(mcp_server.os, "getppid", return_value=12345), \
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
        body = source[idx : idx + 2200]
        assert "_ensure_registered(strict=True)" in body, (
            f"MCP tool {tool} must call _ensure_registered(strict=True) "
            f"before sending; otherwise from_peer can race a hook drop. "
            f"See PR #108 / Issue #107."
        )


@pytest.mark.asyncio
async def test_ensure_registered_does_not_claim_when_multiple_candidates():
    """When MCP has no pane_id (codex sandbox case) and the daemon returns
    multiple online peers matching path+backend, MCP must NOT pick one
    arbitrarily — that's the cross-session identity-theft bug. It should
    fall through to fresh POST /peers and cache the daemon-assigned name.
    See repowire-c6z.
    """
    candidates = [
        {"display_name": "agentbox-codex", "peer_id": "p1"},
        {"display_name": "agentbox-2-codex", "peer_id": "p2"},
    ]
    posted_name = "agentbox-3-codex"
    posted_body = {}

    async def daemon_router(method, url, body=None, params=None):  # noqa: ARG001
        del params
        if method == "GET" and url.startswith("/peers/by-pane/"):
            raise RuntimeError("no pane_id passed in this scenario")
        if method == "GET" and url == "/peers":
            return {"peers": candidates}
        if method == "GET" and url.startswith("/peers/"):
            raise RuntimeError("name lookup miss")
        if method == "POST" and url == "/peers":
            posted_body.update(body or {})
            return {"peer_id": "p3", "display_name": posted_name}
        raise AssertionError(f"unexpected request: {method} {url}")

    with patch.object(
            mcp_server, "get_tmux_info", return_value={"pane_id": None, "session_name": None},
         ), \
         patch.object(mcp_server, "get_pane_id", return_value=None), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)), \
         patch.object(mcp_server, "get_display_name", return_value="agentbox"):
        await mcp_server._ensure_registered()

    assert mcp_server._cached_peer_name == posted_name, (
        "MCP must register fresh when path+backend has multiple online candidates, "
        "not claim one arbitrarily"
    )
    assert mcp_server._cached_peer_name not in {c["display_name"] for c in candidates}
    assert posted_body["circle"] == "default"
    assert posted_body["circle_source"] == "fallback"


@pytest.mark.asyncio
async def test_ensure_registered_marks_explicit_tmux_default_as_tmux():
    """MCP lazy registration must not let explicit tmux "default" look legacy.

    Without circle_source="tmux", PeerRegistry may cross-adopt an old persisted
    non-default mapping for the same path/name/backend.
    """
    posted_body = {}

    async def daemon_router(method, url, body=None, params=None):  # noqa: ARG001
        del params
        if method == "GET" and url.startswith("/peers/by-pane/"):
            raise RuntimeError("pane lookup miss")
        if method == "GET" and url == "/peers":
            return {"peers": []}
        if method == "GET" and url.startswith("/peers/"):
            raise RuntimeError("name lookup miss")
        if method == "POST" and url == "/peers":
            posted_body.update(body or {})
            return {"peer_id": "p3", "display_name": "agentbox-codex"}
        if method == "POST" and url.endswith("/touch"):
            return {"ok": True}
        raise AssertionError(f"unexpected request: {method} {url}")

    with patch.object(
            mcp_server, "get_tmux_info", return_value={"pane_id": "%42", "session_name": "default"},
         ), \
         patch.object(mcp_server, "get_pane_id", return_value=None), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)), \
         patch.object(mcp_server, "get_display_name", return_value="agentbox"):
        await mcp_server._ensure_registered()

    assert posted_body["circle"] == "default"
    assert posted_body["circle_source"] == "tmux"


@pytest.mark.asyncio
async def test_ensure_registered_ignores_by_pane_incumbent_backend_mismatch():
    """Pane lookup may return the live incumbent, not the current MCP process.

    When a temporary same-pane process starts, it should register fresh instead
    of caching the incumbent's peer_id and sending MCP calls as that peer.
    """
    posted_body = {}
    incumbent = {
        "display_name": "orchestrator-codex",
        "peer_id": "repow-orch",
        "path": str(Path.cwd()),
        "backend": "codex",
    }

    async def daemon_router(method, url, body=None, params=None):  # noqa: ARG001
        del params
        if method == "GET" and url.startswith("/peers/by-pane/"):
            return incumbent
        if method == "GET" and url == "/peers":
            return {"peers": []}
        if method == "GET" and url.startswith("/peers/"):
            raise RuntimeError("name lookup miss")
        if method == "POST" and url == "/peers":
            posted_body.update(body or {})
            return {
                "peer_id": "repow-temp",
                "display_name": "project-claude-code",
                "path": str(Path.cwd()),
                "backend": "claude-code",
            }
        if method == "POST" and url.endswith("/touch"):
            return {"ok": True}
        raise AssertionError(f"unexpected request: {method} {url}")

    with patch.object(
            mcp_server, "get_tmux_info", return_value={"pane_id": "%42", "session_name": None},
         ), \
         patch.object(mcp_server, "_detect_backend", return_value="claude-code"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value={
             "display_name": "orchestrator-codex",
             "peer_id": "repow-orch",
             "backend": "codex",
             "agent_pid": 77777,
         }), \
         patch.object(mcp_server, "get_display_name", return_value="project"):
        await mcp_server._ensure_registered()

    assert mcp_server._cached_peer_id == "repow-temp"
    assert mcp_server._cached_peer_name == "project-claude-code"
    assert posted_body["backend"] == "claude-code"
    assert posted_body["pane_id"] == "%42"


@pytest.mark.asyncio
async def test_ensure_registered_skips_path_backend_after_failed_pane_proof():
    """A failed pane proof must block path+backend adoption in the same call."""
    posted_body = {}
    incumbent = {
        "display_name": "orchestrator-codex",
        "peer_id": "repow-orch",
        "path": str(Path.cwd()),
        "backend": "codex",
    }

    async def daemon_router(method, url, body=None, params=None):  # noqa: ARG001
        del params
        if method == "GET" and url.startswith("/peers/by-pane/"):
            return incumbent
        if method == "GET" and url == "/peers":
            raise AssertionError("path+backend fallback must be skipped")
        if method == "GET" and url.startswith("/peers/"):
            raise RuntimeError("name lookup miss")
        if method == "POST" and url == "/peers":
            posted_body.update(body or {})
            return {
                "peer_id": "repow-temp",
                "display_name": "project-codex",
                "path": str(Path.cwd()),
                "backend": "codex",
            }
        if method == "POST" and url.endswith("/touch"):
            return {"ok": True}
        raise AssertionError(f"unexpected request: {method} {url}")

    with patch.object(
            mcp_server, "get_tmux_info", return_value={"pane_id": "%42", "session_name": None},
         ), \
         patch.object(mcp_server, "_detect_backend", return_value="codex"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value={
             "display_name": "orchestrator-codex",
             "peer_id": "repow-orch",
             "backend": "codex",
             "agent_pid": 77777,
         }), \
         patch.object(mcp_server, "get_display_name", return_value="project"):
        await mcp_server._ensure_registered()

    assert mcp_server._cached_peer_id == "repow-temp"
    assert mcp_server._cached_peer_name == "project-codex"
    assert posted_body["backend"] == "codex"
    assert posted_body["pane_id"] == "%42"


@pytest.mark.asyncio
async def test_ensure_registered_claims_when_exactly_one_candidate():
    """The single-candidate case is the legitimate use of path+backend
    fallback: hook-registered peer exists, MCP subprocess started without
    pane env, no ambiguity → reuse the hook-assigned name.
    """
    sole = {"display_name": "agentbox-codex", "peer_id": "p1"}

    async def daemon_router(method, url, body=None, params=None):  # noqa: ARG001
        del body, params
        if method == "GET" and url == "/peers":
            return {"peers": [sole]}
        if method == "GET" and url.startswith("/peers/"):
            raise RuntimeError("name lookup miss")
        raise AssertionError(f"unexpected request: {method} {url} (should not POST)")

    with patch.object(
            mcp_server, "get_tmux_info", return_value={"pane_id": None, "session_name": None},
         ), \
         patch.object(mcp_server, "get_pane_id", return_value=None), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)), \
         patch.object(mcp_server, "get_display_name", return_value="agentbox"):
        await mcp_server._ensure_registered()

    assert mcp_server._cached_peer_name == "agentbox-codex"


@pytest.mark.asyncio
async def test_caches_after_first_resolution():
    with patch.object(mcp_server, "get_pane_id", return_value="%42") as mock_pane, \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=_matching_meta({
             "display_name": "p",
             "peer_id": "x",
         })), \
         patch.object(
             mcp_server,
             "daemon_request",
             new=AsyncMock(return_value=_matching_peer({"display_name": "p", "peer_id": "x"})),
         ):
        await mcp_server._get_my_peer_name()
        await mcp_server._get_my_peer_name()
    # Second call short-circuits via cache
    assert mock_pane.call_count == 1


@pytest.mark.asyncio
async def test_touch_404_invalidates_registration():
    """If the daemon doesn't recognise our cached peer_id (typically because
    it was restarted and our cache is stale), _touch_last_seen must invalidate
    so the next MCP call re-resolves. Without this the stale _cached_peer_id
    is sent as from_peer on ask/notify and ack replies route nowhere.
    """
    mcp_server._cached_peer_id = "stale-peer-id"
    mcp_server._cached_peer_name = "p-name"
    mcp_server._registered = True

    async def daemon_404(method, path, body=None, params=None):
        del method, path, body, params
        raise mcp_server.DaemonHTTPError(404, "Peer not found")

    with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_404)):
        await mcp_server._touch_last_seen()

    assert mcp_server._registered is False
    assert mcp_server._cached_peer_id is None


@pytest.mark.asyncio
async def test_touch_409_invalidates_registration():
    """Same invalidation on 409 ambiguous match (peer reassignment race)."""
    mcp_server._cached_peer_id = "ambiguous-peer-id"
    mcp_server._cached_peer_name = "p-name"
    mcp_server._registered = True

    async def daemon_409(method, path, body=None, params=None):
        del method, path, body, params
        raise mcp_server.DaemonHTTPError(409, "Ambiguous")

    with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_409)):
        await mcp_server._touch_last_seen()

    assert mcp_server._registered is False
    assert mcp_server._cached_peer_id is None


@pytest.mark.asyncio
async def test_touch_other_errors_do_not_invalidate():
    """Connection errors, timeouts, 5xx must NOT invalidate the cache; those
    are transient and the cache is probably still correct.
    """
    mcp_server._cached_peer_id = "real-peer-id"
    mcp_server._cached_peer_name = "p-name"
    mcp_server._registered = True

    async def daemon_500(method, path, body=None, params=None):
        del method, path, body, params
        raise mcp_server.DaemonHTTPError(500, "Internal error")

    with patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_500)):
        await mcp_server._touch_last_seen()

    assert mcp_server._registered is True
    assert mcp_server._cached_peer_id == "real-peer-id"


@pytest.mark.asyncio
async def test_ensure_registered_re_resolves_in_same_call_after_touch_404():
    """After _touch_last_seen 404 invalidates the cache, _ensure_registered
    must continue into the registration path within the SAME call — not wait
    for the next MCP entry. Otherwise the current ask/notify uses stale
    from_peer and the eventual ack reply routes to a nonexistent peer_id.
    """
    mcp_server._cached_peer_id = "stale-id"
    mcp_server._cached_peer_name = "stale-name"
    mcp_server._registered = True

    call_log: list[tuple[str, str]] = []
    # First touch is the stale-id one and 404s; the post-re-registration
    # touch uses the fresh id and should succeed.
    touch_count = {"n": 0}

    async def daemon_router(method, path, body=None, params=None):
        del body, params
        call_log.append((method, path))
        if method == "POST" and "/touch" in path:
            touch_count["n"] += 1
            if touch_count["n"] == 1:
                raise mcp_server.DaemonHTTPError(404, "Peer not found")
            return {"ok": True}
        if method == "GET" and path.startswith("/peers/by-pane/"):
            return _matching_peer({"display_name": "fresh-name", "peer_id": "fresh-id"})
        raise AssertionError(f"unexpected request: {method} {path}")

    tmux_info = {"pane_id": "%99", "session_name": None}
    with patch.object(mcp_server, "get_tmux_info", return_value=tmux_info), \
         patch.object(mcp_server, "get_pane_id", return_value="%99"), \
         patch.object(mcp_server.os, "getppid", return_value=12345), \
         patch.object(mcp_server, "read_pane_runtime_metadata", return_value=_matching_meta({
             "display_name": "fresh-name",
             "peer_id": "fresh-id",
         })), \
         patch.object(mcp_server, "daemon_request", new=AsyncMock(side_effect=daemon_router)):
        await mcp_server._ensure_registered()

    # First the touch (which 404s and invalidates), then by-pane re-resolve.
    assert any("/touch" in path for _, path in call_log), "touch should fire"
    assert any("/peers/by-pane/" in path for _, path in call_log), \
        "re-registration must happen in the same call after touch invalidates"
    assert mcp_server._cached_peer_id == "fresh-id", \
        "post-restart peer_id must be canonicalized in same call, not stale"
    assert mcp_server._cached_peer_name == "fresh-name"
    assert mcp_server._registered is True
