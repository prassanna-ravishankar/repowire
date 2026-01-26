"""Configuration models for Repowire."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Literal

import yaml
from pydantic import BaseModel, Field

# Backend type for pluggable backends
BackendType = Literal["claudemux", "opencode"]


class RelayConfig(BaseModel):
    """Configuration for relay server connection."""

    enabled: bool = Field(default=False, description="Whether to connect to relay")
    url: str = Field(default="wss://relay.repowire.io", description="Relay server URL")
    api_key: str | None = Field(None, description="API key for authentication")


class OpencodeConfig(BaseModel):
    """OpenCode backend settings."""

    default_url: str = Field(
        default="http://localhost:4096", description="Default OpenCode server URL"
    )


class PeerConfig(BaseModel):
    """Configuration for a single peer.

    Identity is based on pane_id (tmux pane ID like "%42") which is unique and stable.
    The name field is kept for backward compatibility with older configs.
    """

    # Primary identity - tmux pane ID (e.g., "%42")
    pane_id: str | None = Field(None, description="Unique tmux pane ID (e.g., '%42')")
    display_name: str | None = Field(None, description="Human-readable name (folder name)")

    # Legacy field - kept for backward compatibility
    name: str = Field(..., description="Peer name (legacy, use display_name)")
    path: str | None = Field(None, description="Working directory path")

    # claudemux backend fields
    tmux_session: str | None = Field(None, description="Tmux session:window")

    # opencode backend fields
    opencode_url: str | None = Field(None, description="OpenCode server URL for this peer")
    session_id: str | None = Field(None, description="Session ID (Claude or OpenCode)")

    # circle (logical subnet)
    circle: str | None = Field(None, description="Circle (logical subnet)")

    # metadata
    metadata: dict = Field(default_factory=dict, description="Additional metadata (e.g., branch)")

    @property
    def effective_name(self) -> str:
        """Get the effective peer name (display_name or fallback to name)."""
        return self.display_name or self.name

    @property
    def effective_pane_id(self) -> str:
        """Get the effective pane_id (or generate legacy placeholder)."""
        if self.pane_id:
            return self.pane_id
        # Generate legacy placeholder for backward compatibility
        if self.tmux_session:
            return f"legacy:{self.tmux_session}"
        return f"legacy:{self.name}"


class DaemonConfig(BaseModel):
    """Configuration for the daemon process."""

    # HTTP daemon settings
    host: str = Field(default="127.0.0.1", description="HTTP daemon host")
    port: int = Field(default=8377, description="HTTP daemon port")
    backend: BackendType = Field(default="claudemux", description="Backend type to use")

    # Legacy/additional settings
    auto_reconnect: bool = Field(default=True, description="Auto-reconnect on disconnect")
    heartbeat_interval: int = Field(default=30, description="Heartbeat interval in seconds")


class LoggingConfig(BaseModel):
    """Logging configuration."""

    level: str = Field(default="info", description="Log level")
    file: str | None = Field(None, description="Log file path")


class Config(BaseModel):
    """Main Repowire configuration."""

    daemon: DaemonConfig = Field(default_factory=DaemonConfig)
    relay: RelayConfig = Field(default_factory=RelayConfig)
    opencode: OpencodeConfig = Field(default_factory=OpencodeConfig)
    peers: dict[str, PeerConfig] = Field(default_factory=dict)  # keyed by peer name
    logging: LoggingConfig = Field(default_factory=LoggingConfig)

    @classmethod
    def get_config_dir(cls) -> Path:
        """Get the Repowire config directory."""
        return Path.home() / ".repowire"

    @classmethod
    def get_config_path(cls) -> Path:
        """Get the config file path."""
        return cls.get_config_dir() / "config.yaml"

    def save(self) -> None:
        """Save configuration to file."""
        config_dir = self.get_config_dir()
        config_dir.mkdir(parents=True, exist_ok=True)

        config_path = self.get_config_path()
        data = self.model_dump()

        with open(config_path, "w") as f:
            yaml.safe_dump(data, f, default_flow_style=False)

    def add_peer(
        self,
        name: str,
        path: str | None = None,
        tmux_session: str | None = None,
        session_id: str | None = None,
        opencode_url: str | None = None,
        circle: str | None = None,
        metadata: dict | None = None,
        pane_id: str | None = None,
        display_name: str | None = None,
    ) -> None:
        """Add or update a peer by name.

        Args:
            name: Peer name (used as config key and legacy identifier)
            path: Working directory path
            tmux_session: Tmux session:window (e.g., 'dev:frontend')
            session_id: Session ID (Claude or OpenCode)
            opencode_url: OpenCode server URL
            circle: Circle (logical subnet)
            metadata: Additional metadata
            pane_id: Unique tmux pane ID (e.g., '%42') - primary identifier
            display_name: Human-readable name (folder name)
        """
        existing = self.peers.get(name)
        # Merge metadata with existing
        merged_metadata = (existing.metadata if existing else {}).copy()
        if metadata:
            merged_metadata.update(metadata)
        self.peers[name] = PeerConfig(
            name=name,
            pane_id=pane_id or (existing.pane_id if existing else None),
            display_name=display_name or (existing.display_name if existing else None),
            path=path or (existing.path if existing else None),
            tmux_session=tmux_session or (existing.tmux_session if existing else None),
            session_id=session_id or (existing.session_id if existing else None),
            opencode_url=opencode_url or (existing.opencode_url if existing else None),
            circle=circle or (existing.circle if existing else None),
            metadata=merged_metadata,
        )
        self.save()

    def update_peer_session(self, name: str, session_id: str) -> bool:
        """Update just the session_id for an existing peer."""
        if name in self.peers:
            self.peers[name].session_id = session_id
            self.save()
            return True
        return False

    def remove_peer(self, name: str) -> bool:
        """Remove a peer by name."""
        if name in self.peers:
            del self.peers[name]
            self.save()
            return True
        return False

    def get_peer(self, name: str) -> PeerConfig | None:
        """Get a peer by name."""
        return self.peers.get(name)

    def get_peer_by_tmux(self, tmux_session: str) -> PeerConfig | None:
        """Get a peer by tmux session:window."""
        for peer in self.peers.values():
            if peer.tmux_session == tmux_session:
                return peer
        return None

    def get_peer_by_pane_id(self, pane_id: str) -> PeerConfig | None:
        """Get a peer by tmux pane ID (e.g., '%42').

        This is the preferred lookup method as pane_id is the primary identifier.
        """
        for peer in self.peers.values():
            if peer.pane_id == pane_id:
                return peer
            # Also check effective_pane_id for legacy configs
            if peer.effective_pane_id == pane_id:
                return peer
        return None


def load_config() -> Config:
    """Load configuration from file or create default."""
    config_path = Config.get_config_path()

    if config_path.exists():
        with open(config_path) as f:
            data = yaml.safe_load(f) or {}
        return Config(**data)

    # Create default config
    config = Config()

    # Check for environment overrides
    if relay_url := os.environ.get("REPOWIRE_RELAY_URL"):
        config.relay.url = relay_url
    if api_key := os.environ.get("REPOWIRE_API_KEY"):
        config.relay.api_key = api_key
        config.relay.enabled = True

    return config
