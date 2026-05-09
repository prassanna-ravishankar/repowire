"""Smoke tests for the OpenCode plugin installer.

The plugin is TypeScript embedded in a Python string. We can't run it from
pytest, but we can assert key APIs are wired up so a future refactor doesn't
silently lose behavior.
"""

from __future__ import annotations

from repowire.installers.opencode import PLUGIN_CONTENT


def test_uses_promptasync_for_queries():
    """Queries fire promptAsync (non-blocking) rather than the old prompt+poll."""
    assert "session.promptAsync" in PLUGIN_CONTENT
    assert "session.message({" not in PLUGIN_CONTENT
    assert "while (Date.now() - start < maxWait)" not in PLUGIN_CONTENT


def test_notifications_use_soft_inject():
    """Notify and broadcast publish a tui.prompt.append event, not session.prompt."""
    assert "tui.prompt.append" in PLUGIN_CONTENT
    assert "tui/publish" in PLUGIN_CONTENT


def test_query_correlation_via_pending_map():
    """Pending queries are correlated by userMessageId (parent), with assistant ID discovered from message.updated parentID."""
    assert "pendingQueries" in PLUGIN_CONTENT
    assert "messageID: userMessageId" in PLUGIN_CONTENT
    assert "pendingByAssistantId" in PLUGIN_CONTENT
    assert "trackAssistantMessage" in PLUGIN_CONTENT
    # Finalization is driven by session.status idle, deferred to handle the
    # part.updated/session.status publish-order race.
    assert "scheduleFlush" in PLUGIN_CONTENT
    assert "flushPendingNow" in PLUGIN_CONTENT
    # Parts arrive on message.part.* events, not on message.updated.
    assert "message.part.updated" in PLUGIN_CONTENT
    assert "message.part.delta" in PLUGIN_CONTENT
    assert "applyPartUpdated" in PLUGIN_CONTENT
    assert "applyPartDelta" in PLUGIN_CONTENT
    # delta event carries new chunk in `delta`, discriminated by field === "text".
    assert 'props.field !== "text"' in PLUGIN_CONTENT
    # Reasoning deltas also use field "text" — filter by tracked text partIDs.
    assert "textPartIds" in PLUGIN_CONTENT


def test_authoritative_busy_idle_via_session_status():
    """session.status is the authoritative busy/idle source (not message.updated heuristic)."""
    assert '"session.status"' in PLUGIN_CONTENT


def test_permission_relay_uses_correct_field_names():
    """permission.ask payload uses `permission` (not `tool`) and canonical sessionID."""
    assert "payload.permission" in PLUGIN_CONTENT


def test_signal_handlers_exit():
    """SIGINT/SIGTERM handlers are one-shot and exit the process (otherwise Node skips default termination)."""
    assert "process.once(\"SIGINT\"" in PLUGIN_CONTENT
    assert "process.once(\"SIGTERM\"" in PLUGIN_CONTENT
    assert "process.exit(130)" in PLUGIN_CONTENT


def test_per_session_peer_registry():
    """Each root session has its own PeerConn (no global primarySessionId singleton)."""
    assert "peerBySession" in PLUGIN_CONTENT
    assert "interface PeerConn" in PLUGIN_CONTENT
    assert "ensurePeer" in PLUGIN_CONTENT
    assert "removePeer" in PLUGIN_CONTENT
    # Old singleton state must be gone.
    assert "let primarySessionId" not in PLUGIN_CONTENT
    assert "resolvePrimarySession" not in PLUGIN_CONTENT


def test_session_events_dispatch_by_session_id():
    """Session and message events route by sessionID to the matching PeerConn."""
    assert "session.created" in PLUGIN_CONTENT
    assert "session.deleted" in PLUGIN_CONTENT
    assert "info.parentID == null" in PLUGIN_CONTENT
    # Inbound message events must look up the peer by sessionID.
    assert "peerBySession.get(info.sessionID)" in PLUGIN_CONTENT


def test_concurrency_guard_per_peer():
    """The plugin rejects concurrent promptAsync calls on the same session."""
    assert "Session busy" in PLUGIN_CONTENT
    assert "conn.busy" in PLUGIN_CONTENT


def test_per_session_peer_id_cache():
    """peer_id cache is keyed by (projectPath, sessionId), not just projectPath."""
    assert "opencode-peer-ids.json" in PLUGIN_CONTENT
    assert "cacheKey" in PLUGIN_CONTENT
    assert "${projectPath}#${sessionId}" in PLUGIN_CONTENT


def test_tools_use_session_context_for_attribution():
    """Tool callbacks use ctx.sessionID to attribute from_peer to the calling session."""
    assert "callerPeer" in PLUGIN_CONTENT
    assert "ctx.sessionID" in PLUGIN_CONTENT
    # ask_peer/notify_peer must pass the per-session peer name as from_peer.
    assert "from_peer: me.peerName" in PLUGIN_CONTENT


def test_system_prompt_names_per_session_identity():
    """experimental.chat.system.transform tells each session its peer name."""
    assert "experimental.chat.system.transform" in PLUGIN_CONTENT
    assert 'You are peer "' in PLUGIN_CONTENT


def test_inbound_notify_prefix_disambiguates_target():
    """Notify/broadcast soft-inject prefixes with target peer name (TUI has one prompt)."""
    assert "@${fromPeer} → ${conn.peerName}:" in PLUGIN_CONTENT


def test_no_session_id_hash_override():
    """Folder name is the stable display name; the old session-ID-hash override is gone."""
    assert "stableNameSet" not in PLUGIN_CONTENT
    assert "info.id.startsWith(\"ses\")" not in PLUGIN_CONTENT


def test_permission_relay_hook_present():
    """permission.ask fires a notify to the telegram peer for relay."""
    assert '"permission.ask"' in PLUGIN_CONTENT
    assert "Permission request:" in PLUGIN_CONTENT
