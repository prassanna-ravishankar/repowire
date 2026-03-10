"""Peer model definitions."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field, model_validator

from repowire.config.models import AgentType


class PeerStatus(str, Enum):
    """Status of a peer in the mesh."""

    ONLINE = "online"
    BUSY = "busy"
    OFFLINE = "offline"


class Peer(BaseModel):
    """A peer in the Repowire mesh.

    A peer represents a Claude Code or OpenCode session that can send and receive messages.

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
        default=AgentType.CLAUDE_CODE, description="Agent type: claude-code or opencode"
    )

    # circle (logical subnet)
    circle: str = Field(default="global", description="Circle (logical subnet)")

    status: PeerStatus = Field(default=PeerStatus.OFFLINE, description="Current status")
    last_seen: datetime | None = Field(None, description="Last activity timestamp")
    metadata: dict[str, Any] = Field(default_factory=dict, description="Additional metadata")

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

    def is_local(self) -> bool:
        """Check if this is a local peer."""
        return self.tmux_session is not None

    def is_claude_code(self) -> bool:
        """Check if this peer runs Claude Code."""
        return self.backend == AgentType.CLAUDE_CODE

    def is_opencode(self) -> bool:
        """Check if this peer runs OpenCode."""
        return self.backend == AgentType.OPENCODE

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
            "status": self.status.value,
            "last_seen": self.last_seen.isoformat() if self.last_seen else None,
            "metadata": self.metadata,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Peer:
        """Create from dictionary."""
        if data.get("last_seen"):
            data["last_seen"] = datetime.fromisoformat(data["last_seen"])
        if data.get("status"):
            data["status"] = PeerStatus(data["status"])
        return cls(**data)
