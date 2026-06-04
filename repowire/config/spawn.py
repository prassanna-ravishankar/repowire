"""Spawn-related configuration models."""

from __future__ import annotations

import shlex

from pydantic import BaseModel, Field, field_validator, model_validator

from repowire.agent_backends import DEFAULT_SPAWN_COMMANDS
from repowire.agent_types import AgentType

__all__ = [
    "DEFAULT_SPAWN_COMMANDS",
    "SpawnProfile",
    "SpawnSettings",
    "apply_spawn_profile",
]


class SpawnProfile(BaseModel):
    """Named spawn command extension for a backend runtime."""

    args: list[str] = Field(
        default_factory=list,
        description="Additional command arguments appended to the backend command",
    )
    description: str | None = Field(
        None,
        description="Human-facing profile description for UIs and docs",
    )


def apply_spawn_profile(base_command: str, profile: SpawnProfile | None) -> str:
    """Return a backend command with profile args appended."""
    if profile is None or not profile.args:
        return base_command
    return f"{base_command} {shlex.join(profile.args)}"


class SpawnSettings(BaseModel):
    """Settings controlling which runtimes and paths agents are allowed to spawn into.

    New configs should use ``commands`` keyed by AgentType value. ``allowed_commands``
    is retained as a one-release compatibility path for older clients/configs.
    ``allowed_paths`` must be non-empty for spawn to be enabled.
    """

    commands: dict[AgentType, str] = Field(
        default_factory=dict,
        description="Spawn command per backend/runtime (empty = spawn disabled)",
    )
    allowed_commands: list[str] = Field(
        default_factory=list,
        description="Deprecated legacy allowed spawn commands",
    )
    allowed_paths: list[str] = Field(
        default_factory=list,
        description="Allowed root directories for spawned sessions (empty = spawn disabled)",
    )
    profiles: dict[AgentType, dict[str, SpawnProfile]] = Field(
        default_factory=dict,
        description="Optional named spawn profiles per backend",
    )
    env_path: list[str] = Field(
        default_factory=list,
        description=(
            "Explicit PATH entries for spawned agent processes. When empty, "
            "Repowire captures the user's login-shell PATH as a fallback."
        ),
    )
    env: dict[str, str] = Field(
        default_factory=dict,
        description="Extra environment variables injected into spawned agent commands",
    )

    @field_validator("env")
    @classmethod
    def validate_env_keys(cls, value: dict[str, str]) -> dict[str, str]:
        for key in value:
            if not key.isidentifier():
                raise ValueError("daemon.spawn.env keys must be shell-safe identifiers")
        return value

    @model_validator(mode="after")
    def _bootstrap_legacy_allowed_commands(self) -> SpawnSettings:
        """Migrate legacy allowed_commands into runtime command profiles on load."""
        if self.commands or not self.allowed_commands:
            return self
        from repowire.agent_backends import AGENT_BACKENDS

        inferred: dict[AgentType, str] = {}
        command_to_backend = {
            cli_name: backend_type
            for backend_type, backend in AGENT_BACKENDS.items()
            for cli_name in backend.cli_names
        }
        for command in self.allowed_commands:
            head = command.split(None, 1)[0] if command else ""
            backend = command_to_backend.get(head)
            if backend and backend not in inferred:
                inferred[backend] = command
        self.commands = inferred
        return self
