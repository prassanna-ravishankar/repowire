"""Peer model definitions."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, Field, model_validator

from repowire.config.models import AgentType

# Per-turn progress, orthogonal to PeerStatus (online/busy/offline liveness).
# - "idle":               Stop hook fired, no turn in progress
# - "working":            UserPromptSubmit fired, mid-turn
# - "awaiting_input":     Notification(idle_prompt) fired -- agent prompt is
#                         live and unconsumed (rate limit, permission, tool
#                         clarification, etc). Orchestrator should respond.
# - "pending_first_turn": Peer was spawned with a seed message but the first
#                         turn never started (spawn-seed drop). Orchestrator
#                         should re-send the brief via notify_peer.
TurnState = Literal["idle", "working", "awaiting_input", "pending_first_turn"]


class PeerRole(str, Enum):
    """Role of a peer in the mesh."""

    AGENT = "agent"
    SERVICE = "service"
    ORCHESTRATOR = "orchestrator"
    HUMAN = "human"


class PeerStatus(str, Enum):
    """Status of a peer in the mesh."""

    ONLINE = "online"
    BUSY = "busy"
    OFFLINE = "offline"


class Peer(BaseModel):
    """A peer in the Repowire mesh.

    A peer represents a Claude Code, OpenCode, Codex, Gemini, or Pi session
    that can send and receive messages.

    Identity is based on a canonical `peer_id` assigned by the daemon's
    SessionMapper on WebSocket connect: `repow-{circle}-{uuid8}`
    (e.g., "repow-dev-a1b2c3d4"). The format is the same for all agent types.

    Message addressing uses `display_name` (human-friendly, may not be unique).
    Internal routing uses `peer_id` (always unique, never ambiguous).
    """

    # Primary identity - daemon-assigned, format: repow-{circle}-{uuid8}
    peer_id: str = Field(..., description="Unique peer identifier (e.g., 'repow-dev-a1b2c3d4')")
    display_name: str = Field(..., description="Human-readable name (folder name)")
    path: str = Field(..., description="Working directory path")
    machine: str = Field(..., description="Machine hostname")

    # tmux session:window for targeting
    tmux_session: str | None = Field(None, description="Tmux session:window (e.g., 'dev:frontend')")
    pane_id: str | None = Field(None, description="Tmux pane ID")

    # Agent type
    backend: AgentType = Field(
        default=AgentType.CLAUDE_CODE,
        description="Agent type: claude-code, opencode, codex, gemini, or pi"
    )

    # circle (logical subnet)
    circle: str = Field(default="global", description="Circle (logical subnet)")

    role: PeerRole = Field(default=PeerRole.AGENT, description="Peer role")

    status: PeerStatus = Field(default=PeerStatus.OFFLINE, description="Current status")
    turn_state: TurnState | None = Field(
        None,
        description=(
            "Per-turn progress (orthogonal to status): idle | working | "
            "awaiting_input | pending_first_turn. None when unknown (e.g. "
            "pre-feature peers, or before any hook has fired)."
        ),
    )
    last_seen: datetime | None = Field(None, description="Last activity timestamp")
    metadata: dict[str, Any] = Field(default_factory=dict, description="Additional metadata")
    description: str = Field(default="", description="Current task description (self-reported)")
    agent_pid: int | None = Field(
        None,
        description=(
            "PID of the agent process that owns this peer (the SessionStart "
            "hook's parent process, i.e. os.getppid() from the hook). Used "
            "by the pane-hijack guard to detect a subprocess agent "
            "registering inside its parent agent's tmux pane."
        ),
    )

    @property
    def name(self) -> str:
        """Backward compatibility: return display_name as name."""
        return self.display_name

    @model_validator(mode="before")
    @classmethod
    def handle_legacy_fields(cls, data: Any) -> Any:
        """Handle legacy field names for backward compatibility.

        Supports:
        - 'name' -> 'display_name' mapping
        - Auto-generate legacy peer_id if not provided
        """
        if isinstance(data, dict):
            # If name is provided but display_name is not, use name as display_name
            if "name" in data and "display_name" not in data:
                data["display_name"] = data["name"]
            # Generate a placeholder peer_id from the best available identifier
            if "peer_id" not in data:
                fallback = data.get("tmux_session") or data.get("display_name") or data.get("name")
                if fallback:
                    data["peer_id"] = f"legacy-{fallback}"
        return data

    @property
    def bypasses_circles(self) -> bool:
        """Whether this peer's role auto-bypasses circle boundaries."""
        return self.role in (PeerRole.SERVICE, PeerRole.ORCHESTRATOR, PeerRole.HUMAN)

    def is_local(self) -> bool:
        """Check if this is a local peer."""
        return self.tmux_session is not None

    def is_claude_code(self) -> bool:
        """Check if this peer runs Claude Code."""
        return self.backend == AgentType.CLAUDE_CODE

    def is_opencode(self) -> bool:
        """Check if this peer runs OpenCode."""
        return self.backend == AgentType.OPENCODE

    def is_codex(self) -> bool:
        """Check if this peer runs Codex."""
        return self.backend == AgentType.CODEX

    def is_gemini(self) -> bool:
        """Check if this peer runs Gemini."""
        return self.backend == AgentType.GEMINI

    def is_pi(self) -> bool:
        """Check if this peer runs Pi."""
        return self.backend == AgentType.PI

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for serialization."""
        return {
            "peer_id": self.peer_id,
            "name": self.display_name,  # API backward compat
            "display_name": self.display_name,
            "path": self.path,
            "machine": self.machine,
            "tmux_session": self.tmux_session,
            "backend": self.backend,
            "circle": self.circle,
            "role": self.role.value,
            "status": self.status.value,
            "turn_state": self.turn_state,
            "last_seen": self.last_seen.isoformat() if self.last_seen else None,
            "metadata": self.metadata,
            "description": self.description,
            "agent_pid": self.agent_pid,
        }
