"""Serialized agent runtime identities."""

from __future__ import annotations

from enum import Enum


class AgentType(str, Enum):
    """Type of AI coding agent a peer is running."""

    CLAUDE_CODE = "claude-code"
    OPENCODE = "opencode"
    CODEX = "codex"
    GEMINI = "gemini"
    ANTIGRAVITY = "antigravity"
    PI = "pi"
    MCP_HTTP = "mcp-http"
