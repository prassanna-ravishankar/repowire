"""Configuration models for Repowire."""

from __future__ import annotations

import os
from pathlib import Path

import yaml
from pydantic import BaseModel, ConfigDict, Field, field_validator
from pydantic_settings import (
    BaseSettings,
    PydanticBaseSettingsSource,
    SettingsConfigDict,
    YamlConfigSettingsSource,
)

from repowire.agent_types import AgentType
from repowire.config.paths import CACHE_DIR, get_config_dir, get_config_path
from repowire.config.relay import RelayConfig
from repowire.config.spawn import (
    DEFAULT_SPAWN_COMMANDS,
    SpawnProfile,
    SpawnSettings,
    apply_spawn_profile,
)

__all__ = [
    "AgentType",
    "CACHE_DIR",
    "Config",
    "DEFAULT_DAEMON_URL",
    "DEFAULT_HOST",
    "DEFAULT_PORT",
    "DEFAULT_QUERY_TIMEOUT",
    "DEFAULT_SPAWN_COMMANDS",
    "DaemonConfig",
    "ExperimentsConfig",
    "OrchestratorRecallConfig",
    "PeerConfig",
    "RelayConfig",
    "SpawnProfile",
    "SpawnSettings",
    "apply_spawn_profile",
    "load_config",
]

DEFAULT_QUERY_TIMEOUT: float = 300.0
"""Default timeout in seconds for peer-to-peer queries (5 minutes)."""

DEFAULT_HOST: str = "127.0.0.1"
DEFAULT_PORT: int = 8377
DEFAULT_DAEMON_URL: str = f"http://{DEFAULT_HOST}:{DEFAULT_PORT}"
"""Default daemon URL used by hooks and MCP server."""


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


class MCPHttpConfig(BaseModel):
    """Opt-in Streamable HTTP MCP endpoint settings."""

    model_config = ConfigDict(extra="ignore")

    enabled: bool = Field(default=False, description="Mount Streamable HTTP MCP at /mcp")
    bind: str = Field(default="localhost-only", description="Only localhost-only is supported")
    require_auth: bool = Field(default=True, description="Require daemon.auth_token bearer auth")
    expose_via_relay: bool = Field(default=False, description="Reserved; /mcp is never relayed")
    allow_unauthenticated_localhost: bool = Field(
        default=False,
        description="Development-only escape hatch when require_auth is disabled",
    )
    allow_dangerous_tools: bool = Field(
        default=False,
        description="Allow lifecycle/admin tools over HTTP MCP",
    )


class OrchestratorRecallConfig(BaseModel):
    """Daemon-side inbound recall triage for orchestrator peers."""

    model_config = ConfigDict(extra="ignore")

    enabled: bool = Field(
        default=True,
        description=(
            "Prefix inbound asks/notifies to role=orchestrator peers with "
            "relevant local orchestrator memory context."
        ),
    )
    max_hits: int = Field(
        default=3,
        ge=1,
        le=5,
        description="Maximum memory hits to inject into one inbound delivery.",
    )
    max_chars: int = Field(
        default=900,
        ge=120,
        le=4000,
        description="Maximum characters in the injected recall block.",
    )
    max_file_chars: int = Field(
        default=12000,
        ge=1000,
        le=100000,
        description="Maximum characters read from any one recall source file.",
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
        default=24, description="Remove session mappings and offline peers older than this",
    )

    # Peer description TTL — clears stale task descriptions on read.
    # Peers tend to forget to clear their set_description after a task ends;
    # this caps the staleness window without requiring a background sweep.
    description_ttl_seconds: float = Field(
        default=900,
        description=(
            "Seconds before a peer's task description is auto-cleared on read. "
            "Set to 0 to disable."
        ),
    )
    peer_reap_ttl_seconds: float = Field(
        default=600,
        description=(
            "Seconds an OFFLINE peer may remain in the registry before lazy repair "
            "reaps it. Set to 0 to disable."
        ),
    )
    stale_busy_timeout_seconds: float = Field(
        default=1800,
        description=(
            "Seconds before lazy repair resets BUSY peers stuck in "
            "turn_state=working to ONLINE/idle after no liveness progress. "
            "Set to 0 to disable."
        ),
    )
    delivery_queue_ttl_seconds: float = Field(
        default=86400,
        description=(
            "Seconds a queued delivery for a polling peer remains drainable. "
            "Set to 0 to disable queued delivery."
        ),
    )
    delivery_queue_max_per_peer: int = Field(
        default=100,
        description=(
            "Maximum queued deliveries retained per peer. Oldest rows are "
            "evicted when the cap is exceeded. Set to 0 to disable."
        ),
    )
    orchestrator_recall: OrchestratorRecallConfig = Field(
        default_factory=OrchestratorRecallConfig,
        description=(
            "Daemon-side inbound recall triage for role=orchestrator peers."
        ),
    )

    # HTTP MCP settings (opt-in)
    mcp_http: MCPHttpConfig = Field(default_factory=MCPHttpConfig)

    # Spawn settings
    spawn: SpawnSettings = Field(default_factory=SpawnSettings)


_VALID_LOG_LEVELS = frozenset({"debug", "info", "warning", "error", "critical"})


class LoggingConfig(BaseModel):
    """Logging configuration."""

    level: str = Field(default="info", description="Log level")
    file: str | None = Field(None, description="Log file path")

    @field_validator("level")
    @classmethod
    def _validate_level(cls, v: str) -> str:
        normalized = v.lower()
        if normalized not in _VALID_LOG_LEVELS:
            raise ValueError(
                f"Invalid log level '{v}'. Must be one of: "
                f"{', '.join(sorted(_VALID_LOG_LEVELS))}"
            )
        return normalized


class TelegramConfig(BaseModel):
    """Telegram bot configuration."""

    bot_token: str | None = Field(None, description="Telegram bot token")
    chat_id: str | None = Field(None, description="Telegram chat ID")


class SlackConfig(BaseModel):
    """Slack bot configuration."""

    bot_token: str | None = Field(None, description="Slack bot token (xoxb-...)")
    app_token: str | None = Field(None, description="Slack app token (xapp-...)")
    channel_id: str | None = Field(None, description="Slack channel ID (C...)")


class UpdatesConfig(BaseModel):
    """Update-check preferences.

    Repowire never mutates its installed package from hooks, MCP calls, daemon
    routing, or background service work. This config only allows explicit
    status/doctor checks to report that a newer package is available.
    """

    model_config = ConfigDict(extra="ignore")

    check_enabled: bool = Field(
        default=False,
        description=(
            "When true, status/doctor may check PyPI for a newer Repowire "
            "release and suggest running `repowire update`. Updates are still "
            "explicit and are never auto-applied."
        ),
    )


# Default tools gated by the PreToolUse remote-approval hook: mutating/shell
# actions only. Read-only tools (Read/Glob/Grep/LS) are intentionally excluded
# so the approval prompt fires on consequential actions, not every file read.
DEFAULT_REMOTE_APPROVAL_TOOLS = ("Bash", "Edit", "Write", "MultiEdit", "NotebookEdit")

# Never gate these read-only tools — approving every file read is pure friction.
# Filtered out of gated_tools even if a user configures them.
READ_ONLY_TOOLS = frozenset({"Read", "Glob", "Grep", "LS"})


class RemoteToolApprovalConfig(BaseModel):
    """PreToolUse hooks-path remote approval (Claude Code).

    When enabled, a PreToolUse hook posts a blocking choice-question to the
    daemon for the gated tools and awaits an allow/deny from a human surface
    (dashboard / Telegram) or a peer, denying on timeout. Off-by-default; the
    installer only registers the hook when ``enabled`` is true.
    """

    model_config = ConfigDict(extra="ignore")

    enabled: bool = Field(default=False, description="Gate gated_tools behind remote approval")
    gated_tools: list[str] = Field(
        default_factory=lambda: list(DEFAULT_REMOTE_APPROVAL_TOOLS),
        description="Tool names that require approval; never gate read-only tools",
    )
    timeout_seconds: float = Field(
        default=45.0,
        description="Requested wait; the daemon clamps to its blocking-question max",
    )

    @field_validator("gated_tools")
    @classmethod
    def _drop_read_only_tools(cls, tools: list[str]) -> list[str]:
        """Read-only tools are never gated, even if configured (codex review)."""
        return [t for t in tools if t not in READ_ONLY_TOOLS]

    @field_validator("timeout_seconds")
    @classmethod
    def _positive_timeout(cls, value: float) -> float:
        """A non-positive timeout would crash the hook HTTP path, not deny cleanly."""
        return value if value > 0 else 45.0


class ExperimentsConfig(BaseModel):
    """Feature flags for experimental subsystems.

    Each flag gates a code path that is not yet ready to be default-on.
    Off-by-default; opt-in via `~/.repowire/config.yaml`:

        experiments:
          acp_broker_client: true
          chat_turn_streaming: true
          remote_tool_approval:
            enabled: true

    Promote to a stable surface or remove once an experiment concludes —
    don't let entries linger.
    """

    model_config = ConfigDict(extra="ignore")

    acp_broker_client: bool = Field(
        default=False,
        description=(
            "Route asks to ACP-marked peers through a broker-side ACP client "
            "(initialize / session/new / session/prompt) instead of the WS path. "
            "Phase-3: codex-acp only."
        ),
    )

    chat_turn_streaming: bool = Field(
        default=False,
        description=(
            "Stream block-level chat_turn_delta events as the assistant turn "
            "is written to the transcript JSONL, in addition to the final "
            "chat_turn at Stop. Claude Code transports only."
        ),
    )

    sqlite_state: bool = Field(
        default=True,
        description=(
            "Deprecated compatibility knob. The daemon state store is SQLite-backed; "
            "legacy JSON files are import-only sources for migrated domains."
        ),
    )

    remote_tool_approval: RemoteToolApprovalConfig = Field(
        default_factory=RemoteToolApprovalConfig,
        description="PreToolUse hooks-path remote tool approval (Claude Code).",
    )


class SkillsConfig(BaseModel):
    """Defaults for the repowire skill pack (cross-agent review/plan/delegate).

    Skills are backend-agnostic markdown; these defaults let an operator pick
    which agent a skill prefers without editing the skill. A skill resolves a
    backend as: explicit arg > the matching default here > a safe fallback
    (ask, or pick a different available backend — never hardcode one). The
    per-skill defaults default to ``default_backend`` when unset.
    """

    model_config = ConfigDict(extra="ignore")

    default_backend: str | None = Field(
        None, description="Fallback backend for skills when no per-skill default is set",
    )
    default_reviewer_backend: str | None = Field(
        None, description="Preferred backend for cross-agent-review",
    )
    default_planner_backend: str | None = Field(
        None, description="Preferred backend for cross-agent-plan",
    )
    default_delegate_backend: str | None = Field(
        None, description="Preferred backend for delegate",
    )
    default_circle: str | None = Field(
        None, description="Default circle for skills that spawn or target peers",
    )

    def resolve(self, key: str) -> str | None:
        """Resolve a per-skill backend key, falling back to default_backend."""
        return getattr(self, key, None) or self.default_backend


class Config(BaseModel):
    """Main Repowire configuration."""

    model_config = ConfigDict(extra="ignore")

    daemon: DaemonConfig = Field(default_factory=DaemonConfig)
    relay: RelayConfig = Field(default_factory=RelayConfig)
    peers: dict[str, PeerConfig] = Field(default_factory=dict)  # legacy, kept for compat
    logging: LoggingConfig = Field(default_factory=LoggingConfig)
    telegram: TelegramConfig = Field(default_factory=TelegramConfig)
    slack: SlackConfig = Field(default_factory=SlackConfig)
    updates: UpdatesConfig = Field(default_factory=UpdatesConfig)
    experiments: ExperimentsConfig = Field(default_factory=ExperimentsConfig)
    skills: SkillsConfig = Field(default_factory=SkillsConfig)

    @classmethod
    def get_config_dir(cls) -> Path:
        """Get the Repowire config directory."""
        return get_config_dir()

    @classmethod
    def get_config_path(cls) -> Path:
        """Get the config file path."""
        return get_config_path()

    def save(self) -> None:
        """Save configuration to file atomically (write tmp + rename, 0600 perms)."""
        config_dir = self.get_config_dir()
        config_dir.mkdir(parents=True, exist_ok=True)

        config_path = self.get_config_path()
        tmp_path = config_path.with_suffix(".yaml.tmp")
        data = self.model_dump(mode="json")

        with open(tmp_path, "w") as f:
            yaml.safe_dump(data, f, default_flow_style=False)
        tmp_path.chmod(0o600)
        tmp_path.replace(config_path)

    def get_peer(self, name: str) -> PeerConfig | None:
        """Get a peer by name (legacy config lookup)."""
        return self.peers.get(name)


class _RelayEnvCompatSource(PydanticBaseSettingsSource):
    """Relay env compatibility source: flat aliases + the ``enabled`` side-effect.

    Before pydantic-settings, ``load_config`` read ``REPOWIRE_RELAY_URL`` and
    ``REPOWIRE_API_KEY`` directly (only when no config file existed) and set
    ``relay.enabled=True`` when an api_key was present. We keep those flat names
    working (alongside the nested ``REPOWIRE_RELAY__URL`` form) as a dedicated
    source rather than a model_validator, so plain ``Config(**data)`` stays
    env-insensitive.

    The ``enabled`` side-effect fires for *either* spelling of the api key: the
    flat ``REPOWIRE_API_KEY`` or the canonical nested ``REPOWIRE_RELAY__API_KEY``.
    The nested key's *value* is still supplied by ``env_settings``; this source
    only contributes the ``enabled`` flag for it, so adopting the canonical env
    spelling does not leave the relay configured-but-disabled.
    """

    def get_field_value(self, field, field_name):  # noqa: D102 - source protocol
        return None, field_name, False

    def __call__(self) -> dict[str, object]:
        # This source ranks above env_settings, so a flat REPOWIRE_RELAY_URL wins
        # over the nested REPOWIRE_RELAY__URL if both are set (legacy compat).
        relay: dict[str, object] = {}
        if url := os.environ.get("REPOWIRE_RELAY_URL"):
            relay["url"] = url
        if api_key := os.environ.get("REPOWIRE_API_KEY"):
            relay["api_key"] = api_key
        if os.environ.get("REPOWIRE_API_KEY") or os.environ.get("REPOWIRE_RELAY__API_KEY"):
            relay["enabled"] = True
        return {"relay": relay} if relay else {}


class _LoadedConfig(Config, BaseSettings):
    """Settings-resolving view of Config, used only by ``load_config()``.

    ``Config`` itself stays a pure value object (deterministic, env/yaml-free) so
    every ``Config(...)`` call site and test fixture is unaffected. This subclass
    layers the resolution sources; ``load_config`` returns it typed as ``Config``.
    Precedence (highest first): init kwargs > flat relay aliases > env
    (``REPOWIRE_*``, nested via ``__``) > yaml file > model defaults.
    """

    model_config = SettingsConfigDict(
        env_prefix="REPOWIRE_",
        env_nested_delimiter="__",
        extra="ignore",
    )

    @classmethod
    def settings_customise_sources(
        cls, settings_cls, init_settings, env_settings, dotenv_settings,
        file_secret_settings,
    ):
        yaml_source = YamlConfigSettingsSource(settings_cls, yaml_file=Config.get_config_path())
        return (
            init_settings,
            _RelayEnvCompatSource(settings_cls),
            env_settings,
            yaml_source,
            file_secret_settings,
        )


def load_config() -> Config:
    """Load configuration, layering env vars over the yaml file over defaults.

    Resolution (highest precedence first): env (flat ``REPOWIRE_RELAY_URL`` /
    ``REPOWIRE_API_KEY`` aliases and nested ``REPOWIRE_*__*``) > the
    ``~/.repowire/config.yaml`` file > model defaults. Note: env now *overrides*
    the yaml file (previously env was applied only when no file existed).
    """
    return Config.model_validate(_LoadedConfig().model_dump())
