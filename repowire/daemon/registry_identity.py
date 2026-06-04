"""Identity helpers and durable mapping types for the peer registry."""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Literal

from repowire.agent_types import AgentType
from repowire.protocol.peers import PeerRole

CircleSource = Literal["tmux", "spawn_hint", "fallback"]


def normalize_identity_path(raw: str) -> str:
    """Canonical path form for peer identity matching."""
    if not raw:
        return ""
    try:
        return os.path.normpath(os.path.realpath(raw))
    except (OSError, ValueError):
        return os.path.normpath(raw)


def is_configured_orchestrator_path(raw: str | None) -> bool:
    """Return True when ``raw`` is the configured orchestrator workspace path."""
    if not raw:
        return False
    try:
        from repowire.orchestrator.workspace import workspace_path

        expected = normalize_identity_path(str(workspace_path()))
    except Exception:  # noqa: BLE001 - path check must fail closed
        return False
    return normalize_identity_path(raw) == expected


@dataclass
class SessionMapping:
    """Persistent mapping of session to peer identity."""

    session_id: str  # "repow-dev-a1b2c3d4"
    display_name: str
    circle: str
    backend: AgentType
    path: str | None = None
    role: PeerRole = PeerRole.AGENT
    updated_at: str | None = None
    description: str = ""
    model: str | None = None
    # Pid of the agent process that last owned this peer. Persisted so the
    # pane-hijack guard survives daemon restart.
    agent_pid: int | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.backend, AgentType):
            self.backend = AgentType(self.backend)
        if not isinstance(self.role, PeerRole):
            self.role = PeerRole(self.role)
        if self.updated_at is None:
            self.updated_at = datetime.now(timezone.utc).isoformat()
