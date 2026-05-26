"""Shared daemon spawn service used by HTTP routes and durable job runner."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from fastapi import HTTPException, status

from repowire.agent_backends import AgentResumePlan, build_resume_command
from repowire.config.models import AgentType, Config, apply_spawn_profile
from repowire.installers.post_spawn import post_spawn_warmup
from repowire.protocol.peers import PeerRole
from repowire.spawn import SpawnConfig, SpawnResult, spawn_peer
from repowire.spawn_ownership import record_spawn_ownership


@dataclass(frozen=True)
class SpawnServiceResult:
    display_name: str
    tmux_session: str
    pane_id: str | None
    message: str | None


class SpawnService:
    """Spawn peers while preserving /spawn validation and ownership semantics."""

    def __init__(
        self,
        *,
        config: Config,
        spawned_pane_ids: set[str] | None = None,
        background_tasks: set[asyncio.Task] | None = None,
        spawn_impl: Callable[[SpawnConfig], SpawnResult] = spawn_peer,
        warmup_impl=post_spawn_warmup,
    ) -> None:
        self._config = config
        self._spawned_pane_ids = spawned_pane_ids if spawned_pane_ids is not None else set()
        self._background_tasks = background_tasks if background_tasks is not None else set()
        self._spawn_impl = spawn_impl
        self._warmup_impl = warmup_impl

    def validate_path(self, path: str) -> str:
        allowed_paths = self._config.daemon.spawn.allowed_paths
        commands = self._config.daemon.spawn.commands
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
        command = self._config.daemon.spawn.commands.get(backend)
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
            selected_profile = self._config.daemon.spawn.profiles.get(backend, {}).get(profile)
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
        try:
            result = self._spawn_impl(
                SpawnConfig(
                    path=resolved_path,
                    circle=circle,
                    backend=backend,
                    command=command,
                    message=message,
                    role=role.value,
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
