"""Smoke tests for the Pi extension installer.

The extension is TypeScript embedded in a Python string. We can't run it
from pytest, but we can assert key APIs are wired up so a future refactor
doesn't silently lose behavior.
"""

from __future__ import annotations

from pathlib import Path

from repowire.installers.pi import (
    EXTENSION_FILENAME,
    PLUGIN_CONTENT,
)

# -- TS surface --------------------------------------------------------------


def test_extension_default_export_is_async_factory():
    """Pi auto-loads extensions as `export default function (pi) { ... }`."""
    assert "export default async function repowireExtension" in PLUGIN_CONTENT
    assert "ExtensionAPI" in PLUGIN_CONTENT


def test_session_start_filters_by_reason():
    """Peer registration happens only on reason startup or new, not resume/reload/fork."""
    assert 'reason !== "startup"' in PLUGIN_CONTENT
    assert 'reason !== "new"' in PLUGIN_CONTENT
    assert "session_start" in PLUGIN_CONTENT


def test_session_shutdown_removes_peer():
    assert "session_shutdown" in PLUGIN_CONTENT
    assert "removePeer(sessionId)" in PLUGIN_CONTENT


def test_session_before_compact_scaffold():
    """The pre-compact hook is registered as a no-op scaffold for v1."""
    assert "session_before_compact" in PLUGIN_CONTENT


def test_turn_end_flushes_pending():
    """turn_end is the canonical finalize event for pi pending queries."""
    assert "turn_end" in PLUGIN_CONTENT
    assert "flushPending" in PLUGIN_CONTENT


def test_message_update_buffers_text_deltas():
    """Streaming text_deltas (not thinking_deltas) buffer into the pending query."""
    assert "message_update" in PLUGIN_CONTENT
    assert "assistantMessageEvent" in PLUGIN_CONTENT
    assert 'ame.type === "text_delta"' in PLUGIN_CONTENT
    assert "pending.buffer.push(ame.delta)" in PLUGIN_CONTENT


def test_message_update_captures_stream_errors():
    """assistantMessageEvent type 'error' surfaces as a query error."""
    assert 'ame.type === "error"' in PLUGIN_CONTENT
    assert "pending.hasError = true" in PLUGIN_CONTENT


def test_session_id_from_session_manager():
    """Session id is read from ctx.sessionManager.getSessionId, not the event payload."""
    assert "getSessionId" in PLUGIN_CONTENT
    assert "ctx.sessionManager" in PLUGIN_CONTENT


def test_soft_inject_uses_send_user_message():
    """Inbound asks/notify/broadcast surface via pi.sendUserMessage with steer."""
    assert "piApi.sendUserMessage" in PLUGIN_CONTENT
    assert 'deliverAs: "steer"' in PLUGIN_CONTENT


def test_soft_inject_branches_on_idle_state():
    """When agent is idle, omit deliverAs; while streaming, use steer."""
    assert "piCtx.isIdle()" in PLUGIN_CONTENT
    # Idle branch sends bare text; streaming branch passes deliverAs steer.
    assert "piApi.sendUserMessage(text);" in PLUGIN_CONTENT


def test_typebox_schema_for_tools():
    """Tool parameters use TypeBox Type.Object, not bare JSON-Schema."""
    assert 'from "@sinclair/typebox"' in PLUGIN_CONTENT
    assert "Type.Object({" in PLUGIN_CONTENT
    assert "Type.String(" in PLUGIN_CONTENT
    assert "Type.Optional(" in PLUGIN_CONTENT


def test_ctx_capture_for_isidle():
    """Event handlers capture ctx so soft-inject can branch on isIdle()."""
    assert "function capture(" in PLUGIN_CONTENT
    assert "piCtx = ctx" in PLUGIN_CONTENT


def test_caller_peer_uses_session_manager():
    """callerPeer reads active session from ctx.sessionManager.getSessionId for attribution."""
    assert "sessionManager" in PLUGIN_CONTENT
    assert "getSessionId" in PLUGIN_CONTENT


def test_tools_have_label_field():
    """Pi 0.74 ToolDefinition requires a `label` field for UI display."""
    # Every registerTool block needs a label. Cheap heuristic: count
    # registerTool calls vs label entries inside them.
    register_count = PLUGIN_CONTENT.count("pi.registerTool(")
    label_count = PLUGIN_CONTENT.count('label: "Repowire:')
    assert register_count == label_count, (
        f"label_count ({label_count}) != registerTool count ({register_count})"
    )
    assert register_count >= 8


def test_tool_results_include_details_field():
    """AgentToolResult requires `details`; set undefined for tools without structured details."""
    # Each tool execute returns at least one result object with details: undefined.
    assert "details: undefined" in PLUGIN_CONTENT
    # Heuristic: at least as many details fields as tool definitions.
    assert PLUGIN_CONTENT.count("details: undefined") >= 8


def test_ask_is_framed_with_correlation_id():
    """Inbound asks must include [ask #cid] framing so the agent can ack."""
    assert "[ask #" in PLUGIN_CONTENT


def test_per_session_peer_registry():
    """Each root pi session has its own PeerConn (no global singleton)."""
    assert "peerBySession" in PLUGIN_CONTENT
    assert "interface PeerConn" in PLUGIN_CONTENT
    assert "ensurePeer" in PLUGIN_CONTENT
    assert "removePeer" in PLUGIN_CONTENT


def test_per_session_peer_id_cache():
    """peer_id cache is keyed by (projectPath, sessionId) at the pi-specific path."""
    assert "pi-peer-ids.json" in PLUGIN_CONTENT
    assert "cacheKey" in PLUGIN_CONTENT


def test_concurrency_guard_per_peer():
    """The extension rejects concurrent queries on the same session."""
    assert "Session busy" in PLUGIN_CONTENT
    assert "conn.busy" in PLUGIN_CONTENT
    assert "activeTurnCorrelationId" in PLUGIN_CONTENT


def test_websocket_connect_carries_backend_pi():
    """Daemon connect message identifies the runtime as pi."""
    assert 'backend: "pi"' in PLUGIN_CONTENT


def test_reconnect_with_backoff():
    """Disconnects schedule a bounded exponential backoff reconnect."""
    assert "schedulePeerReconnect" in PLUGIN_CONTENT
    assert "MAX_RECONNECT_ATTEMPTS" in PLUGIN_CONTENT


def test_signal_handlers_exit():
    """SIGINT/SIGTERM handlers are one-shot and exit, mirroring opencode."""
    assert 'process.once("SIGINT"' in PLUGIN_CONTENT
    assert 'process.once("SIGTERM"' in PLUGIN_CONTENT
    assert "process.exit(130)" in PLUGIN_CONTENT
    assert "process.exit(143)" in PLUGIN_CONTENT


def test_tools_registered():
    """All canonical mesh tools are registered."""
    tools = (
        "list_peers", "ask", "ack", "notify_peer",
        "broadcast", "whoami", "set_description", "set_circle",
    )
    for name in tools:
        assert 'name: "' + name + '"' in PLUGIN_CONTENT, f"tool {name} not registered"


def test_tools_use_registerTool_api():
    """Tools are registered via pi.registerTool(), not opencode's tool() factory."""
    assert "pi.registerTool(" in PLUGIN_CONTENT
    # Opencode's tool factory pattern must not have leaked in.
    assert "@opencode-ai/plugin" not in PLUGIN_CONTENT


def test_tmux_pane_discovery():
    """The extension derives circle and pane id from tmux when available."""
    assert "TMUX_PANE" in PLUGIN_CONTENT
    assert "display-message" in PLUGIN_CONTENT


# -- Filesystem install/uninstall -------------------------------------------


def test_install_extension_writes_to_global_path(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    # Re-import to pick up the patched home.
    from repowire.installers import pi as pi_mod
    target = tmp_path / ".pi" / "agent" / "extensions" / EXTENSION_FILENAME
    # Recompute the install path under the patched home.
    monkeypatch.setattr(pi_mod, "GLOBAL_EXTENSION_DIR", tmp_path / ".pi" / "agent" / "extensions")
    assert pi_mod.install_extension(global_install=True) is True
    assert target.exists()
    assert "repowireExtension" in target.read_text()


def test_uninstall_extension_removes_file(tmp_path, monkeypatch):
    from repowire.installers import pi as pi_mod
    monkeypatch.setattr(pi_mod, "GLOBAL_EXTENSION_DIR", tmp_path / ".pi" / "agent" / "extensions")
    pi_mod.install_extension(global_install=True)
    assert pi_mod.uninstall_extension(global_install=True) is True
    assert pi_mod.uninstall_extension(global_install=True) is False  # already gone


def test_check_extension_installed(tmp_path, monkeypatch):
    from repowire.installers import pi as pi_mod
    monkeypatch.setattr(pi_mod, "GLOBAL_EXTENSION_DIR", tmp_path / ".pi" / "agent" / "extensions")
    assert pi_mod.check_extension_installed(global_install=True) is False
    pi_mod.install_extension(global_install=True)
    assert pi_mod.check_extension_installed(global_install=True) is True


def test_local_install_writes_to_dot_pi(tmp_path, monkeypatch):
    """Local install puts extension into .pi/extensions/ relative to cwd."""
    monkeypatch.chdir(tmp_path)
    from repowire.installers import pi as pi_mod
    assert pi_mod.install_extension(global_install=False) is True
    assert (tmp_path / ".pi" / "extensions" / EXTENSION_FILENAME).exists()


# -- AgentType wiring -------------------------------------------------------


def test_agent_type_pi_registered():
    """PI enum value and command default are wired up so spawn paths resolve."""
    from repowire.config.models import AgentType
    from repowire.spawn import AGENT_COMMANDS

    assert AgentType.PI.value == "pi"
    assert AGENT_COMMANDS[AgentType.PI] == "pi"


def test_command_to_backend_includes_pi():
    """Spawn route's command->backend map auto-derives pi entry."""
    from repowire.config.models import AgentType
    from repowire.daemon.routes.spawn import _COMMAND_TO_BACKEND

    assert _COMMAND_TO_BACKEND.get("pi") == AgentType.PI
