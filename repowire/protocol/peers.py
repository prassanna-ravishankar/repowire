"""Peer model definitions."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field


class PeerStatus(str, Enum):
    """Status of a peer in the mesh."""

    ONLINE = "online"
    BUSY = "busy"
    OFFLINE = "offline"


class Peer(BaseModel):
    """A peer in the Repowire mesh.

    A peer represents a Claude Code or OpenCode session that can send and receive messages.
    """

    name: str = Field(..., description="Human-readable peer name (e.g., 'frontend', 'backend')")
    path: str = Field(..., description="Working directory path")
    machine: str = Field(..., description="Machine hostname")

    # claudemux backend fields
    tmux_session: str | None = Field(None, description="Tmux session name (for claudemux peers)")

    # opencode backend fields
    opencode_url: str | None = Field(None, description="OpenCode server URL (for opencode peers)")
    session_id: str | None = Field(None, description="OpenCode session ID")

    # circle (logical subnet)
    circle: str = Field(default="global", description="Circle (logical subnet)")

    status: PeerStatus = Field(default=PeerStatus.OFFLINE, description="Current status")
    last_seen: datetime | None = Field(None, description="Last activity timestamp")
    metadata: dict[str, Any] = Field(default_factory=dict, description="Additional metadata")

    def is_local(self) -> bool:
        """Check if this is a local peer (tmux-based or local opencode)."""
        return self.tmux_session is not None or (
            self.opencode_url is not None and "localhost" in self.opencode_url
        )

    def is_claudemux(self) -> bool:
        """Check if this peer uses the claudemux backend."""
        return self.tmux_session is not None

    def is_opencode(self) -> bool:
        """Check if this peer uses the opencode backend."""
        return self.opencode_url is not None

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for serialization."""
        return {
            "name": self.name,
            "path": self.path,
            "machine": self.machine,
            "tmux_session": self.tmux_session,
            "opencode_url": self.opencode_url,
            "session_id": self.session_id,
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
