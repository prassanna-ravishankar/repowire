"""Shared daemon spawn service used by HTTP routes and durable job runner."""

from __future__ import annotations

import asyncio
import os
import shlex
import shutil
import subprocess
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from fastapi import HTTPException, status

from repowire.agent_backends import AgentResumePlan, build_resume_command
from repowire.config.models import AgentType, SpawnSettings, apply_spawn_profile
from repowire.daemon.spawn_diagnostics import capture_spawn_registration_diagnostics
from repowire.installers.post_spawn import post_spawn_warmup
from repowire.protocol.peers import PeerRole
from repowire.spawn import SpawnConfig, SpawnResult, kill_pane, spawn_peer
from repowire.spawn_ownership import forget_spawn_ownership, record_spawn_ownership


@dataclass(frozen=True)
class SpawnServiceResult:
    display_name: str
    tmux_session: str
    pane_id: str | None
    message: str | None
    command: str | None = None
    env: dict[str, str] | None = None


@dataclass(frozen=True)
class SpawnRegistrationResult:
    spawn_result: SpawnServiceResult | None
    resolved_peer: Any | None
    registration_attempts: list[dict[str, Any]]


class SpawnAttemptError(Exception):
    def __init__(
        self,
        original: Exception,
        *,
        attempt: int,
        registration_attempts: list[dict[str, Any]],
    ) -> None:
        super().__init__(str(original))
        self.original = original
        self.attempt = attempt
        self.registration_attempts = registration_attempts


class SpawnService:
    """Spawn peers while preserving /spawn validation and ownership semantics."""

    def __init__(
        self,
        *,
        spawn: SpawnSettings,
        spawned_pane_ids: set[str] | None = None,
        background_tasks: set[asyncio.Task] | None = None,
        spawn_impl: Callable[[SpawnConfig], SpawnResult] = spawn_peer,
        warmup_impl=post_spawn_warmup,
    ) -> None:
        self._spawn = spawn
        self._spawned_pane_ids = spawned_pane_ids if spawned_pane_ids is not None else set()
        self._background_tasks = background_tasks if background_tasks is not None else set()
        self._spawn_impl = spawn_impl
        self._warmup_impl = warmup_impl

    def validate_path(self, path: str) -> str:
        allowed_paths = self._spawn.allowed_paths
        commands = self._spawn.commands
        if not commands or not allowed_paths:
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail=(
                    "Spawn is disabled. Set daemon.spawn.commands and "
                    "daemon.spawn.allowed_paths in ~/.repowire/config.yaml"
                ),
            )
        resolved = Path(path).expanduser().resolve()
        if not resolved.exists():
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND, detail=f"Path does not exist: {path}"
            )
        roots = [Path(p).expanduser().resolve() for p in allowed_paths]
        if not any(resolved == root or root in resolved.parents for root in roots):
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail=f"Path not under any allowed_paths: {path}",
            )
        return str(resolved)

    def resolve_command(self, backend: AgentType, profile: str | None = None) -> str:
        command = self._spawn.commands.get(backend)
        if not command:
            raise HTTPException(
                status_code=status.HTTP_422_UNPROCESSABLE_CONTENT,
                detail={
                    "error": "command_unavailable",
                    "hint": (
                        f"No daemon.spawn.commands entry for {backend.value!r}. "
                        "Add it to ~/.repowire/config.yaml."
                    ),
                    "backend": backend.value,
                },
            )
        if profile:
            selected_profile = self._spawn.profiles.get(backend, {}).get(profile)
            if selected_profile is None:
                raise HTTPException(
                    status_code=status.HTTP_422_UNPROCESSABLE_CONTENT,
                    detail={
                        "error": "profile_unavailable",
                        "hint": (
                            f"No daemon.spawn.profiles.{backend.value}.{profile} "
                            "entry in ~/.repowire/config.yaml."
                        ),
                        "backend": backend.value,
                        "profile": profile,
                    },
                )
            command = apply_spawn_profile(command, selected_profile)
        return command

    def spawn(
        self,
        *,
        path: str,
        backend: AgentType,
        profile: str | None = None,
        circle: str = "default",
        message: str | None = None,
        role: PeerRole = PeerRole.AGENT,
        peer_id: str | None = None,
        resume_plan: AgentResumePlan | None = None,
    ) -> SpawnServiceResult:
        resolved_path = self.validate_path(path)
        command = self.resolve_command(backend, profile)
        if resume_plan is not None:
            command = self.resume_command(command, backend=backend, resume_plan=resume_plan)
        env = self.spawn_env()
        self.validate_command_env(command, env)
        try:
            result = self._spawn_impl(
                SpawnConfig(
                    path=resolved_path,
                    circle=circle,
                    backend=backend,
                    command=command,
                    message=message,
                    role=role.value,
                    peer_id=peer_id,
                    env=env,
                )
            )
        except (ValueError, RuntimeError) as e:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail=str(e)
            ) from e

        if result.pane_id:
            self._spawned_pane_ids.add(result.pane_id)
            record_spawn_ownership(
                pane_id=result.pane_id,
                path=resolved_path,
                backend=backend,
                circle=circle,
                role=role,
                display_name=result.display_name,
                tmux_session=result.tmux_session,
                peer_id=peer_id,
            )
            task = asyncio.create_task(
                self._warmup_impl(
                    backend,
                    result.pane_id,
                    path=resolved_path,
                    circle=circle,
                    message=result.message,
                )
            )
            self._background_tasks.add(task)
            task.add_done_callback(self._background_tasks.discard)

        return SpawnServiceResult(
            display_name=result.display_name,
            tmux_session=result.tmux_session,
            pane_id=result.pane_id,
            message=result.message,
            command=command,
            env=env,
        )

    async def spawn_registered(
        self,
        *,
        path: str,
        backend: AgentType,
        await_peer: Callable[[SpawnServiceResult], Awaitable[Any | None]],
        profile: str | None = None,
        circle: str = "default",
        message: str | None = None,
        role: PeerRole = PeerRole.AGENT,
        peer_id: str | None = None,
        resume_plan: AgentResumePlan | None = None,
        attempts: int = 2,
        on_spawned: Callable[[SpawnServiceResult], None] | None = None,
    ) -> SpawnRegistrationResult:
        """Spawn and wait for the peer to self-register, retrying timed-out panes."""
        registration_attempts: list[dict[str, Any]] = []
        spawn_result: SpawnServiceResult | None = None
        for attempt in range(1, attempts + 1):
            try:
                spawn_result = self.spawn(
                    path=path,
                    backend=backend,
                    profile=profile,
                    circle=circle,
                    message=message,
                    role=role,
                    peer_id=peer_id,
                    resume_plan=resume_plan,
                )
            except Exception as exc:
                raise SpawnAttemptError(
                    exc,
                    attempt=attempt,
                    registration_attempts=registration_attempts,
                ) from exc
            if on_spawned is not None:
                on_spawned(spawn_result)
            resolved = await await_peer(spawn_result)
            if resolved is not None:
                return SpawnRegistrationResult(
                    spawn_result=spawn_result,
                    resolved_peer=resolved,
                    registration_attempts=registration_attempts,
                )
            diagnostics = await capture_spawn_registration_diagnostics(
                pane_id=spawn_result.pane_id,
                command=spawn_result.command,
                env=spawn_result.env,
            )
            diagnostics["attempt"] = attempt
            registration_attempts.append(diagnostics)
            if spawn_result.pane_id:
                kill_pane(spawn_result.pane_id)
                forget_spawn_ownership(spawn_result.pane_id)

        return SpawnRegistrationResult(
            spawn_result=spawn_result,
            resolved_peer=None,
            registration_attempts=registration_attempts,
        )

    def spawn_env(self) -> dict[str, str]:
        """Return explicit environment injected into spawned agent commands."""
        env = {key: str(value) for key, value in self._spawn.env.items()}
        if "PATH" not in env:
            if self._spawn.env_path:
                env["PATH"] = os.pathsep.join(
                    str(Path(entry).expanduser()) for entry in self._spawn.env_path
                )
            else:
                captured = capture_login_shell_path()
                if captured:
                    env["PATH"] = captured
        return env

    def validate_command_env(self, command: str, env: dict[str, str]) -> None:
        """Fail fast when an explicitly configured PATH cannot find the command head."""
        explicit_path = bool(self._spawn.env_path or "PATH" in self._spawn.env)
        if not explicit_path:
            return
        command_head = command_head_for_probe(command)
        if command_head is None:
            return
        path = env.get("PATH") or os.environ.get("PATH") or ""
        if shutil.which(command_head, path=path):
            return
        raise HTTPException(
            status_code=status.HTTP_422_UNPROCESSABLE_CONTENT,
            detail={
                "error": "command_unavailable",
                "command": command_head,
                "path": path,
                "hint": (
                    "The configured spawn PATH cannot find the backend command. "
                    "Update daemon.spawn.env_path or daemon.spawn.commands."
                ),
            },
        )

    @staticmethod
    def resume_command(
        command: str,
        *,
        backend: AgentType,
        resume_plan: AgentResumePlan,
    ) -> str:
        """Return a backend-native resume command for a recorded runtime session."""
        if resume_plan.backend != backend:
            raise HTTPException(
                status_code=status.HTTP_422_UNPROCESSABLE_CONTENT,
                detail={
                    "error": "resume_backend_mismatch",
                    "backend": backend.value,
                    "resume_backend": resume_plan.backend.value,
                },
            )
        try:
            return build_resume_command(command, resume_plan)
        except ValueError as e:
            raise HTTPException(
                status_code=status.HTTP_422_UNPROCESSABLE_CONTENT,
                detail={
                    "error": "backend_resume_unavailable",
                    "backend": backend.value,
                    "hint": str(e),
                },
            ) from e


def capture_login_shell_path() -> str | None:
    """Capture PATH from the user's login shell for launchd/tmux-spawned agents."""
    shell = os.environ.get("SHELL") or "/bin/zsh"
    start = "REPOWIRE_PATH_START:"
    end = ":REPOWIRE_PATH_END"
    try:
        result = subprocess.run(
            [shell, "-lc", f'printf "{start}%s{end}" "$PATH"'],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    output = result.stdout
    start_idx = output.rfind(start)
    if start_idx < 0:
        return None
    path_start = start_idx + len(start)
    end_idx = output.find(end, path_start)
    if end_idx < 0:
        return None
    path = output[path_start:end_idx].strip()
    return path or None


def command_head_for_probe(command: str) -> str | None:
    """Return a simple executable head for PATH probing, or None for complex shell."""
    command = command.rsplit("&&", 1)[-1].strip()
    if any(token in command for token in ("||", ";", "|", "$(", "`", "<", ">")):
        return None
    try:
        parts = shlex.split(command)
    except ValueError:
        return None
    if not parts:
        return None
    head = parts[0]
    if head in {"alias", "cd", "command", "eval", "exec", "export", "source"}:
        return None
    return head
