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

    A peer represents a Claude Code session that can send and receive messages.
    """

    name: str = Field(..., description="Human-readable peer name (e.g., 'frontend', 'backend')")
    path: str = Field(..., description="Working directory path")
    machine: str = Field(..., description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session name (for local peers)")
    status: PeerStatus = Field(default=PeerStatus.OFFLINE, description="Current status")
    last_seen: datetime | None = Field(None, description="Last activity timestamp")
    metadata: dict[str, Any] = Field(default_factory=dict, description="Additional metadata")

    def is_local(self) -> bool:
        """Check if this is a local peer (tmux-based)."""
        return self.tmux_session is not None

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for serialization."""
        return {
            "name": self.name,
            "path": self.path,
            "machine": self.machine,
            "tmux_session": self.tmux_session,
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
