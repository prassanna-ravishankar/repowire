"""Configuration models for Repowire."""

from __future__ import annotations

import os
from enum import Enum
from pathlib import Path

import yaml
from pydantic import BaseModel, ConfigDict, Field

DEFAULT_QUERY_TIMEOUT: float = 300.0
"""Default timeout in seconds for peer-to-peer queries (5 minutes)."""

CACHE_DIR: Path = Path.home() / ".cache" / "repowire"
"""Runtime cache directory for logs and transient state."""

DEFAULT_HOST: str = "127.0.0.1"
DEFAULT_PORT: int = 8377
DEFAULT_DAEMON_URL: str = f"http://{DEFAULT_HOST}:{DEFAULT_PORT}"
"""Default daemon URL used by hooks and MCP server."""


class AgentType(str, Enum):
    """Type of AI coding agent a peer is running."""

    CLAUDE_CODE = "claude-code"
    OPENCODE = "opencode"


class RelayConfig(BaseModel):
    """Configuration for relay server connection."""

    enabled: bool = Field(default=False, description="Whether to connect to relay")
    url: str = Field(default="wss://repowire.io", description="Relay server URL")
    api_key: str | None = Field(None, description="API key for authentication")

    @property
    def dashboard_url(self) -> str | None:
        """Dashboard URL via the relay, or None if not configured."""
        if not self.api_key:
            return None
        return "https://repowire.io/dashboard"

    def ensure_api_key(self) -> str:
        """Register with relay and set API key if missing. Returns the key."""
        if self.api_key:
            return self.api_key
        import getpass

        import httpx

        relay_http = self.url.replace("wss://", "https://")
        user_id = getpass.getuser()
        resp = httpx.post(f"{relay_http}/api/v1/register", json={"user_id": user_id})
        resp.raise_for_status()
        self.api_key = resp.json()["api_key"]
        return self.api_key


class PeerConfig(BaseModel):
    """Configuration for a single peer.

    Identity is based on a canonical `peer_id` assigned by the daemon's
    SessionMapper on WebSocket connect: `repow-{circle}-{uuid8}`
    (e.g., "repow-dev-a1b2c3d4"). The format is the same for all agent types.

    The name field is kept for backward compatibility with older configs.
    """

    model_config = ConfigDict(extra="ignore")

    # Primary identity - daemon-assigned, format: repow-{circle}-{uuid8}
    peer_id: str | None = Field(None, description="Unique peer ID (e.g., 'repow-dev-a1b2c3d4')")
    display_name: str | None = Field(None, description="Human-readable name (folder name)")

    # Legacy field - kept for backward compatibility
    name: str = Field(..., description="Peer name (legacy, use display_name)")
    path: str | None = Field(None, description="Working directory path")

    # Claude Code fields
    tmux_session: str | None = Field(None, description="Tmux session:window")

    # circle (logical subnet)
    circle: str | None = Field(None, description="Circle (logical subnet)")

    # metadata
    metadata: dict = Field(default_factory=dict, description="Additional metadata (e.g., branch)")

    @property
    def effective_name(self) -> str:
        """Get the effective peer name (display_name or fallback to name)."""
        return self.display_name or self.name

    @property
    def effective_peer_id(self) -> str:
        """Get the effective peer_id (or generate legacy placeholder)."""
        if self.peer_id:
            return self.peer_id
        # Generate legacy placeholder for backward compatibility
        # Use hyphen instead of colon to avoid issues
        if self.tmux_session:
            return f"legacy-{self.tmux_session}"
        return f"legacy-{self.name}"


class SpawnSettings(BaseModel):
    """Settings controlling which commands and paths agents are allowed to spawn into.

    Both allowed_commands and allowed_paths must be non-empty for spawn to be enabled.
    A spawn request must match an entry in each list to proceed.
    """

    allowed_commands: list[str] = Field(
        default_factory=list,
        description="Allowed spawn commands (empty = spawn disabled)",
    )
    allowed_paths: list[str] = Field(
        default_factory=list,
        description="Allowed root directories for spawned sessions (empty = spawn disabled)",
    )


class DaemonConfig(BaseModel):
    """Configuration for the daemon process."""

    # HTTP daemon settings
    host: str = Field(default="127.0.0.1", description="HTTP daemon host")
    port: int = Field(default=8377, description="HTTP daemon port")

    # Security settings
    auth_token: str | None = Field(
        None, description="Authentication token for WebSocket connections"
    )

    # Legacy/additional settings
    auto_reconnect: bool = Field(default=True, description="Auto-reconnect on disconnect")
    heartbeat_interval: int = Field(default=30, description="Heartbeat interval in seconds")

    # Session cleanup
    prune_max_age_hours: float = Field(
        default=72, description="Remove session mappings older than this on startup",
    )

    # Spawn settings
    spawn: SpawnSettings = Field(default_factory=SpawnSettings)


class LoggingConfig(BaseModel):
    """Logging configuration."""

    level: str = Field(default="info", description="Log level")
    file: str | None = Field(None, description="Log file path")


class Config(BaseModel):
    """Main Repowire configuration."""

    model_config = ConfigDict(extra="ignore")

    daemon: DaemonConfig = Field(default_factory=DaemonConfig)
    relay: RelayConfig = Field(default_factory=RelayConfig)
    peers: dict[str, PeerConfig] = Field(default_factory=dict)  # legacy, kept for compat
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

    def get_peer(self, name: str) -> PeerConfig | None:
        """Get a peer by name (legacy config lookup)."""
        return self.peers.get(name)


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
