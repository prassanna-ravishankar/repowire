"""Precedence regressions for detect_mcp_backend (repowire-ueq).

The MCP backend detector layers: explicit REPOWIRE_BACKEND > pid-guarded pane
metadata > weak runtime markers (claude/gemini before the codex PATH heuristic).
These pin that explicit/metadata signals beat ambient runtime-env leakage — the
class of bug that made the old TestMcpRegistration tests fail inside a Claude
Code session (fixed in #330).
"""

from __future__ import annotations

from repowire.agent_backends import detect_mcp_backend
from repowire.config.models import AgentType


def test_explicit_backend_beats_ambient_claude_markers() -> None:
    # A Codex MCP launched from a shell carrying Claude Code session vars must
    # still detect codex, because the installer writes explicit REPOWIRE_BACKEND.
    env = {
        "REPOWIRE_BACKEND": "codex",
        "CLAUDECODE": "1",
        "CLAUDE_CODE_SESSION_ID": "abc",
        "AI_AGENT": "claude-code",
    }
    assert detect_mcp_backend(env, current_agent_pid=None) is AgentType.CODEX


def test_explicit_backend_beats_codex_path_for_claude() -> None:
    # Symmetric: explicit claude-code wins even when a .codex/ path is inherited.
    env = {"REPOWIRE_BACKEND": "claude-code", "PATH": "/home/u/.codex/bin"}
    assert detect_mcp_backend(env, current_agent_pid=None) is AgentType.CLAUDE_CODE


def test_codex_path_heuristic_is_weak_and_loses_to_claude_markers() -> None:
    # Documents the intentional precedence: with no explicit signal, ambient
    # Claude markers beat the weak ".codex/ in PATH" heuristic. (This is exactly
    # why backend-detection tests must null the other runtimes' markers.)
    env = {"PATH": "/home/u/.codex/bin", "CLAUDECODE": "1"}
    assert detect_mcp_backend(env, current_agent_pid=None) is AgentType.CLAUDE_CODE


def test_codex_path_detected_when_no_competing_signal() -> None:
    env = {"PATH": "/home/u/.codex/bin"}
    assert detect_mcp_backend(env, current_agent_pid=None) is AgentType.CODEX


def test_defaults_to_claude_code_with_no_signal() -> None:
    assert detect_mcp_backend({}, current_agent_pid=None) is AgentType.CLAUDE_CODE
