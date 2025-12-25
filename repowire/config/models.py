"""Configuration models for Repowire."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field


class RelayConfig(BaseModel):
    """Configuration for relay server connection."""

    enabled: bool = Field(default=False, description="Whether to connect to relay")
    url: str = Field(default="wss://relay.repowire.io", description="Relay server URL")
    api_key: str | None = Field(None, description="API key for authentication")


class PeerConfig(BaseModel):
    """Configuration for a single peer."""

    tmux_session: str = Field(..., description="Tmux session name")
    path: str = Field(..., description="Working directory path")


class DaemonConfig(BaseModel):
    """Configuration for the daemon process."""

    auto_reconnect: bool = Field(default=True, description="Auto-reconnect on disconnect")
    heartbeat_interval: int = Field(default=30, description="Heartbeat interval in seconds")
    socket_path: str = Field(default="/tmp/repowire.sock", description="Unix socket path for IPC")


class LoggingConfig(BaseModel):
    """Logging configuration."""

    level: str = Field(default="info", description="Log level")
    file: str | None = Field(None, description="Log file path")


class Config(BaseModel):
    """Main Repowire configuration."""

    relay: RelayConfig = Field(default_factory=RelayConfig)
    peers: dict[str, PeerConfig] = Field(default_factory=dict)
    daemon: DaemonConfig = Field(default_factory=DaemonConfig)
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

    def add_peer(self, name: str, tmux_session: str, path: str) -> None:
        """Add a peer to configuration."""
        self.peers[name] = PeerConfig(tmux_session=tmux_session, path=path)
        self.save()

    def remove_peer(self, name: str) -> bool:
        """Remove a peer from configuration."""
        if name in self.peers:
            del self.peers[name]
            self.save()
            return True
        return False


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
