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
    # The old polling loop should be gone.
    assert "session.message({" not in PLUGIN_CONTENT
    assert "while (Date.now() - start < maxWait)" not in PLUGIN_CONTENT


def test_notifications_use_soft_inject():
    """Notify and broadcast publish a tui.prompt.append event, not session.prompt."""
    assert "tui.prompt.append" in PLUGIN_CONTENT
    assert "tui/publish" in PLUGIN_CONTENT


def test_query_correlation_via_pending_map():
    """Pending queries are correlated via a map keyed on pre-generated message IDs."""
    assert "pendingQueries" in PLUGIN_CONTENT
    assert "messageID: messageId" in PLUGIN_CONTENT
    assert "resolvePendingQuery" in PLUGIN_CONTENT


def test_session_events_filter_by_parentid():
    """Subagent sub-sessions (parentID set) shouldn't clobber primarySessionId."""
    assert "parentID == null" in PLUGIN_CONTENT
    assert "primarySessionId" in PLUGIN_CONTENT


def test_no_session_id_hash_override():
    """Folder name is the stable display name; the old session-ID-hash override is gone."""
    assert "stableNameSet" not in PLUGIN_CONTENT
    assert "info.id.startsWith(\"ses\")" not in PLUGIN_CONTENT


def test_permission_relay_hook_present():
    """permission.ask fires a notify to the telegram peer for relay."""
    assert '"permission.ask"' in PLUGIN_CONTENT
    assert "Permission request:" in PLUGIN_CONTENT
