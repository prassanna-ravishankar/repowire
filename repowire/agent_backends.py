"""Agent backend behavior registry.

AgentType is the serialized identity used in config, APIs, and state. AgentBackend
owns backend-specific behavior and delegates heavyweight implementation to the
runtime-specific installer/config modules.

Keep this module independent of repowire.config.models at import time. Config
models re-export DEFAULT_SPAWN_COMMANDS for compatibility, so module-level
config imports would create a cycle.
"""

from __future__ import annotations

import shlex
import shutil
from abc import ABC
from dataclasses import dataclass
from pathlib import Path
from typing import Any, ClassVar

from repowire.agent_types import AgentType


@dataclass(frozen=True)
class AgentResumePlan:
    backend: AgentType
    runtime_session_id: str
    repowire_session_id: str | None = None
    capability: dict | None = None


@dataclass(frozen=True)
class BackendInstallOptions:
    use_channels: bool = False


@dataclass(frozen=True)
class BackendInstallMessage:
    level: str
    text: str


@dataclass(frozen=True)
class McpConfigScope:
    owner: str
    effective_scope: str
    label: str
    description: str
    supported_scopes: tuple[str, ...] = ("user",)
    default_scope: str = "user"
    is_global: bool = True

    def as_dict(self, backend: AgentType) -> dict[str, Any]:
        return {
            "backend": backend.value,
            "owner": self.owner,
            "effective_scope": self.effective_scope,
            "label": self.label,
            "description": self.description,
            "supported_scopes": list(self.supported_scopes),
            "default_scope": self.default_scope,
            "is_global": self.is_global,
        }


class AgentBackend(ABC):
    agent_type: ClassVar[AgentType]
    display_name: ClassVar[str]
    cli_names: ClassVar[tuple[str, ...]] = ()
    config_markers: ClassVar[tuple[Path, ...]] = ()
    default_command: ClassVar[str | None] = None
    local_cli: ClassVar[bool] = True
    supports_local_spawn: ClassVar[bool] = True
    supports_resume: ClassVar[bool] = False
    resume_strategy: ClassVar[str] = "unsupported"
    resume_flag: ClassVar[str | None] = None
    resume_subcommand: ClassVar[str | None] = None
    mcp_config_scope: ClassVar[McpConfigScope | None] = None
    post_spawn_strategy: ClassVar[str] = "seed_message"

    def detect_installed(self) -> bool:
        return any(shutil.which(name) for name in self.cli_names) or any(
            marker.exists() for marker in self.config_markers
        )

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        raise NotImplementedError(f"Setup is not implemented for {self.agent_type.value}")

    def resume_capability(self, runtime_session_id: str | None) -> dict:
        if not runtime_session_id:
            return {}
        if not self.supports_resume:
            return {
                "supported": False,
                "strategy": self.resume_strategy,
                "reason": "backend_resume_not_implemented",
            }
        return {
            "supported": True,
            "strategy": self.resume_strategy,
            "runtime_session_id_arg": runtime_session_id,
        }

    def can_resume(self, runtime_session_id: str | None) -> bool:
        return self.supports_resume and bool(runtime_session_id)

    def build_resume_command(self, command: str, runtime_session_id: str) -> str:
        if not self.supports_resume:
            raise ValueError(
                f"Backend-native resume is not available for {self.agent_type.value}"
            )
        quoted_session = shlex.quote(runtime_session_id)
        if self.resume_subcommand:
            return f"{command} {self.resume_subcommand} {quoted_session}"
        return f"{command} {self.resume_flag} {quoted_session}"

    async def post_spawn_warmup(
        self,
        pane_id: str,
        *,
        path: str,
        circle: str,
        message: str | None = None,
    ) -> None:
        if self.post_spawn_strategy == "codex_warmup":
            from repowire.installers.post_spawn import DEFAULT_WARMUP_TEMPLATE, _codex_warmup

            text = message or DEFAULT_WARMUP_TEMPLATE.format(path=path, circle=circle)
            await _codex_warmup(pane_id, text)
        elif message:
            from repowire.installers.post_spawn import _claude_code_family_seed

            await _claude_code_family_seed(pane_id, message)

    def list_mcp_servers(self, peer):
        if self.mcp_config_scope is None:
            self._raise_mcp_unsupported()
        raise NotImplementedError

    def add_mcp_server(self, peer, spec) -> None:
        if self.mcp_config_scope is None:
            self._raise_mcp_unsupported()
        raise NotImplementedError

    def remove_mcp_server(self, peer, name: str) -> None:
        if self.mcp_config_scope is None:
            self._raise_mcp_unsupported()
        raise NotImplementedError

    def _raise_mcp_unsupported(self) -> None:
        from repowire.peer_mcp import NotSupportedError

        raise NotSupportedError(f"MCP config not supported for backend {self.agent_type.value}")


class ClaudeCodeBackend(AgentBackend):
    agent_type = AgentType.CLAUDE_CODE
    display_name = "Claude Code"
    cli_names = ("claude",)
    default_command = "claude --dangerously-skip-permissions"
    supports_resume = True
    resume_strategy = "claude_resume"
    resume_flag = "--resume"
    mcp_config_scope = McpConfigScope(
        owner="peer/project",
        effective_scope="peer_project",
        label="Claude Code peer/project config",
        description=(
            "Claude Code MCP edits can target user/global config or the peer's "
            "project/worktree via the selected add scope."
        ),
        supported_scopes=("user", "project"),
        is_global=False,
    )

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        import subprocess

        from repowire.installers.claude_code import install_channel, install_hooks

        options = options or BackendInstallOptions()
        messages: list[BackendInstallMessage] = []
        if options.use_channels:
            channel_ok, channel_msg = install_channel()
            if channel_ok:
                install_hooks(channel_mode=True)
                messages.append(BackendInstallMessage("success", channel_msg))
            else:
                install_hooks()
                messages.append(BackendInstallMessage("warning", channel_msg))
                messages.append(
                    BackendInstallMessage(
                        "success",
                        "Claude Code hooks installed (fallback)",
                    )
                )
        else:
            install_hooks()
            messages.append(BackendInstallMessage("success", "Claude Code hooks installed"))

        try:
            subprocess.run(["claude", "mcp", "remove", "repowire"], capture_output=True)
            cmd = ["claude", "mcp", "add", "-s", "user", "repowire", "--", "repowire", "mcp"]
            subprocess.run(cmd, check=True)
            messages.append(BackendInstallMessage("success", "MCP server added to Claude"))
        except subprocess.CalledProcessError as e:
            messages.append(
                BackendInstallMessage(
                    "error",
                    f"Failed to configure Claude MCP server: {e}",
                )
            )
        return messages

    def list_mcp_servers(self, peer):
        from repowire import peer_mcp

        return peer_mcp._claude_list(peer)

    def add_mcp_server(self, peer, spec) -> None:
        from repowire import peer_mcp

        peer_mcp._claude_add(peer, spec)

    def remove_mcp_server(self, peer, name: str) -> None:
        from repowire import peer_mcp

        peer_mcp._claude_remove(peer, name)


class CodexBackend(AgentBackend):
    agent_type = AgentType.CODEX
    display_name = "Codex"
    cli_names = ("codex",)
    default_command = "codex --dangerously-bypass-approvals-and-sandbox"
    supports_resume = True
    resume_strategy = "codex_resume"
    resume_subcommand = "resume"
    post_spawn_strategy = "codex_warmup"
    mcp_config_scope = McpConfigScope(
        owner="backend",
        effective_scope="backend_global",
        label="Codex global backend config",
        description=(
            "Codex MCP edits target the user-level Codex config shared by "
            "Codex sessions on this host."
        ),
    )

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        from repowire.installers.codex import install_hooks, install_mcp

        messages: list[BackendInstallMessage] = []
        try:
            install_hooks()
            messages.append(BackendInstallMessage("success", "Codex hooks installed"))
        except Exception as e:
            messages.append(BackendInstallMessage("error", f"Failed to install Codex hooks: {e}"))
        try:
            install_mcp()
            messages.append(BackendInstallMessage("success", "Codex MCP server configured"))
        except Exception as e:
            messages.append(BackendInstallMessage("error", f"Failed to configure Codex MCP: {e}"))
        return messages

    def list_mcp_servers(self, peer):
        from repowire import peer_mcp

        return peer_mcp._codex_list()

    def add_mcp_server(self, peer, spec) -> None:
        from repowire import peer_mcp

        peer_mcp._codex_add(spec)

    def remove_mcp_server(self, peer, name: str) -> None:
        from repowire import peer_mcp

        peer_mcp._codex_remove(name)


class GeminiBackend(AgentBackend):
    agent_type = AgentType.GEMINI
    display_name = "Gemini"
    cli_names = ("gemini",)
    default_command = "gemini --yolo"
    supports_resume = True
    resume_strategy = "gemini_resume"
    resume_flag = "--resume"
    mcp_config_scope = McpConfigScope(
        owner="backend",
        effective_scope="backend_global",
        label="Gemini global backend config",
        description=(
            "Gemini MCP edits target the user-level Gemini settings shared by "
            "Gemini sessions on this host."
        ),
    )

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        from repowire.installers.gemini import install_hooks, install_mcp

        messages: list[BackendInstallMessage] = []
        try:
            install_hooks()
            messages.append(BackendInstallMessage("success", "Gemini hooks installed"))
        except Exception as e:
            messages.append(BackendInstallMessage("error", f"Failed to install Gemini hooks: {e}"))
        try:
            install_mcp()
            messages.append(BackendInstallMessage("success", "Gemini MCP server configured"))
        except Exception as e:
            messages.append(BackendInstallMessage("error", f"Failed to configure Gemini MCP: {e}"))
        return messages

    def list_mcp_servers(self, peer):
        from repowire import peer_mcp

        return peer_mcp._gemini_list()

    def add_mcp_server(self, peer, spec) -> None:
        from repowire import peer_mcp

        peer_mcp._gemini_add(spec)

    def remove_mcp_server(self, peer, name: str) -> None:
        from repowire import peer_mcp

        peer_mcp._gemini_remove(name)


class OpenCodeBackend(AgentBackend):
    agent_type = AgentType.OPENCODE
    display_name = "OpenCode"
    cli_names = ("opencode",)
    config_markers = (Path.home() / ".config" / "opencode",)
    default_command = "opencode"
    supports_resume = True
    resume_strategy = "opencode_session"
    resume_flag = "--session"

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        from repowire.installers.opencode import install_plugin

        try:
            install_plugin()
            return [BackendInstallMessage("success", "OpenCode plugin installed")]
        except Exception as e:
            return [BackendInstallMessage("error", f"Failed to install OpenCode plugin: {e}")]


class AntigravityBackend(AgentBackend):
    agent_type = AgentType.ANTIGRAVITY
    display_name = "Antigravity"
    cli_names = ("agy",)
    config_markers = (Path.home() / ".gemini" / "antigravity-cli",)
    default_command = "agy --dangerously-skip-permissions"
    supports_resume = True
    resume_strategy = "antigravity_conversation"
    resume_flag = "--conversation"

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        from repowire.installers.antigravity import install_hooks

        try:
            install_hooks()
            return [
                BackendInstallMessage("success", "Antigravity plugin installed"),
                BackendInstallMessage(
                    "info",
                    "Hook firing and MCP registration via `agy` plugins are "
                    "pending upstream verification.",
                ),
            ]
        except Exception as e:
            return [BackendInstallMessage("error", f"Failed to install Antigravity plugin: {e}")]


class PiBackend(AgentBackend):
    agent_type = AgentType.PI
    display_name = "Pi"
    cli_names = ("pi",)
    config_markers = (Path.home() / ".pi",)
    default_command = "pi"
    supports_resume = True
    resume_strategy = "pi_session"
    resume_flag = "--session"

    def install(self, options: BackendInstallOptions | None = None) -> list[BackendInstallMessage]:
        from repowire.installers.pi import install_extension

        try:
            install_extension()
            return [BackendInstallMessage("success", "Pi extension installed")]
        except Exception as e:
            return [BackendInstallMessage("error", f"Failed to install Pi extension: {e}")]


class McpHttpBackend(AgentBackend):
    agent_type = AgentType.MCP_HTTP
    display_name = "HTTP MCP"
    local_cli = False
    supports_local_spawn = False
    default_command = None


AGENT_BACKENDS: dict[AgentType, AgentBackend] = {
    AgentType.CLAUDE_CODE: ClaudeCodeBackend(),
    AgentType.OPENCODE: OpenCodeBackend(),
    AgentType.CODEX: CodexBackend(),
    AgentType.GEMINI: GeminiBackend(),
    AgentType.ANTIGRAVITY: AntigravityBackend(),
    AgentType.PI: PiBackend(),
    AgentType.MCP_HTTP: McpHttpBackend(),
}

DEFAULT_SPAWN_COMMANDS: dict[AgentType, str] = {
    backend_type: backend.default_command
    for backend_type, backend in AGENT_BACKENDS.items()
    if backend.default_command is not None
}


def agent_backend_for(backend: AgentType) -> AgentBackend:
    return AGENT_BACKENDS[backend]


def resume_capability_for_registration(
    backend: AgentType,
    runtime_session_id: str | None,
) -> dict:
    return agent_backend_for(backend).resume_capability(runtime_session_id)


def can_resume_backend(
    backend: AgentType,
    *,
    runtime_session_id: str | None = None,
) -> bool:
    return agent_backend_for(backend).can_resume(runtime_session_id)


def build_resume_command(command: str, plan: AgentResumePlan) -> str:
    return agent_backend_for(plan.backend).build_resume_command(
        command,
        plan.runtime_session_id,
    )
