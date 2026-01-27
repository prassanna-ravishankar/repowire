"""Peer model definitions."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field, model_validator

from repowire.config.models import BackendType


class PeerStatus(str, Enum):
    """Status of a peer in the mesh."""

    ONLINE = "online"
    BUSY = "busy"
    OFFLINE = "offline"


class Peer(BaseModel):
    """A peer in the Repowire mesh.

    A peer represents a Claude Code or OpenCode session that can send and receive messages.
    Identity is based on tmux pane ID (e.g., "%42") which is unique and stable.
    """

    # Primary identity - tmux pane ID (e.g., "%42")
    pane_id: str = Field(..., description="Unique tmux pane ID (e.g., '%42')")
    display_name: str = Field(..., description="Human-readable name (folder name)")
    path: str = Field(..., description="Working directory path")
    machine: str = Field(..., description="Machine hostname")

    # tmux session:window for targeting
    tmux_session: str | None = Field(None, description="Tmux session:window (e.g., 'dev:frontend')")

    # Backend type
    backend: BackendType = Field(
        default="claudemux", description="Backend type: claudemux or opencode"
    )

    # Legacy/optional fields
    opencode_url: str | None = Field(None, description="OpenCode server URL (for opencode peers)")
    session_id: str | None = Field(None, description="Session ID")

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
    def handle_legacy_name(cls, data: Any) -> Any:
        """Handle legacy 'name' field for backward compatibility."""
        if isinstance(data, dict):
            # If name is provided but display_name is not, use name as display_name
            if "name" in data and "display_name" not in data:
                data["display_name"] = data["name"]
            # If pane_id is not provided, generate a placeholder from tmux_session or name
            if "pane_id" not in data:
                if data.get("tmux_session"):
                    data["pane_id"] = f"legacy:{data['tmux_session']}"
                elif data.get("display_name"):
                    data["pane_id"] = f"legacy:{data['display_name']}"
                elif data.get("name"):
                    data["pane_id"] = f"legacy:{data['name']}"
        return data

    def is_local(self) -> bool:
        """Check if this is a local peer (tmux-based or local opencode)."""
        return self.tmux_session is not None or (
            self.opencode_url is not None and "localhost" in self.opencode_url
        )

    def is_claudemux(self) -> bool:
        """Check if this peer uses the claudemux backend."""
        return self.backend == "claudemux"

    def is_opencode(self) -> bool:
        """Check if this peer uses the opencode backend."""
        return self.backend == "opencode"

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for serialization."""
        return {
            "pane_id": self.pane_id,
            "name": self.display_name,  # API backward compat
            "display_name": self.display_name,
            "path": self.path,
            "machine": self.machine,
            "tmux_session": self.tmux_session,
            "backend": self.backend,
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
