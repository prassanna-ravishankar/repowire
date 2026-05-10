"""Spawn endpoints — create and kill agent sessions via tmux."""

from __future__ import annotations

import asyncio
from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_config, get_peer_registry
from repowire.daemon.peer_registry import PeerRegistry
from repowire.installers.post_spawn import post_spawn_warmup
from repowire.spawn import AGENT_COMMANDS, SpawnConfig, SpawnResult, kill_peer, spawn_peer

_COMMAND_TO_BACKEND: dict[str, AgentType] = {
    cmd: backend for backend, cmd in AGENT_COMMANDS.items()
}

# Strong references to background warmup tasks. asyncio holds only weak refs to
# tasks, so without this set a long-sleeping warmup can be GC'd mid-flight.
_BACKGROUND_TASKS: set[asyncio.Task] = set()


def _backend_from_command(command: str) -> AgentType:
    """Derive AgentType from the first token of the command string."""
    head = command.split(None, 1)[0] if command else ""
    return _COMMAND_TO_BACKEND.get(head, AgentType.CLAUDE_CODE)

router = APIRouter(tags=["spawn"])


class SpawnConfigResponse(BaseModel):
    """Spawn configuration for UI discovery."""

    enabled: bool
    allowed_commands: list[str] = []
    allowed_paths: list[str] = []


@router.get("/spawn/config", response_model=SpawnConfigResponse)
async def get_spawn_config(
    _: str | None = Depends(require_auth),
) -> SpawnConfigResponse:
    """Return spawn configuration so the UI can offer spawn controls."""
    cfg = get_config()
    cmds = cfg.daemon.spawn.allowed_commands
    paths = cfg.daemon.spawn.allowed_paths
    return SpawnConfigResponse(
        enabled=bool(cmds and paths),
        allowed_commands=cmds,
        allowed_paths=paths,
    )


class SpawnRequest(BaseModel):
    """Request to spawn a new agent session."""

    path: str = Field(..., description="Absolute path to the project directory")
    command: str = Field(..., description="Command to run — must be in allowed_commands")
    circle: str = Field(default="default", description="Circle to spawn into")
    message: str | None = Field(
        default=None,
        description=(
            "Optional warmup message to send to the spawned agent. Used by "
            "backends whose hook lifecycle requires a first prompt (codex). "
            "Other backends ignore it. Default: a short branded warmup."
        ),
    )


class SpawnResponse(BaseModel):
    """Result of a successful spawn."""

    ok: bool = True
    display_name: str
    tmux_session: str


class KillResponse(BaseModel):
    """Result of a successful kill."""

    ok: bool = True


class KillPeerRequest(BaseModel):
    """Request to kill a registered peer by mesh identity."""

    peer_identifier: str = Field(..., description="Peer ID or display name from /peers")
    circle: str | None = Field(None, description="Circle to scope display-name lookup")
    from_peer: str | None = Field(
        None, description="Caller peer_id or display_name — used for role-based authorization"
    )


async def _authorize_kill(registry: PeerRegistry, from_peer: str | None) -> None:
    """Hook for future role-based authorization (e.g. orchestrator-only kill)."""


def _validate_spawn_request(path: str, command: str) -> None:
    """Validate path and command against the spawn allowlists.

    Raises HTTPException 403 if spawn is disabled or either value is not allowed.
    Raises HTTPException 404 if the path does not exist on disk.
    """
    cfg = get_config()
    allowed_commands = cfg.daemon.spawn.allowed_commands
    allowed_paths = cfg.daemon.spawn.allowed_paths

    if not allowed_commands or not allowed_paths:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail=(
                "Spawn is disabled. Set daemon.spawn.allowed_commands and"
                " daemon.spawn.allowed_paths in ~/.repowire/config.yaml"
            ),
        )

    if command not in allowed_commands:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail=f"Command not in allowed_commands: {command!r}",
        )

    resolved = Path(path).expanduser().resolve()
    if not resolved.exists():
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Path does not exist: {path}",
        )

    allowed_roots = [Path(p).expanduser().resolve() for p in allowed_paths]
    if not any(resolved == root or root in resolved.parents for root in allowed_roots):
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail=f"Path not under any allowed_paths: {path}",
        )


@router.post("/spawn", response_model=SpawnResponse)
async def spawn(
    request: SpawnRequest,
    _: str | None = Depends(require_auth),
) -> SpawnResponse:
    """Spawn a new agent coding session.

    Both the command and the path must be explicitly allowed in
    daemon.spawn.allowed_commands / allowed_paths in ~/.repowire/config.yaml.
    The spawned agent self-registers via its SessionStart hook once it starts.
    """
    _validate_spawn_request(request.path, request.command)

    resolved_path = str(Path(request.path).expanduser().resolve())
    backend = _backend_from_command(request.command)

    try:
        result: SpawnResult = spawn_peer(
            SpawnConfig(
                path=resolved_path,
                circle=request.circle,
                backend=backend,
                command=request.command,
                message=request.message,
            )
        )
    except (ValueError, RuntimeError) as e:
        raise HTTPException(status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail=str(e))

    # Schedule post-spawn warmup in the background -- the codex case sleeps
    # ~10s and would otherwise stall the /spawn response. claude/opencode/gemini
    # warmups are no-ops and return immediately. Hold a strong ref to the task
    # in a module-level set so asyncio doesn't GC it mid-sleep.
    if result.pane_id:
        task = asyncio.create_task(
            post_spawn_warmup(
                backend,
                result.pane_id,
                path=resolved_path,
                circle=request.circle,
                message=result.message,
            )
        )
        _BACKGROUND_TASKS.add(task)
        task.add_done_callback(_BACKGROUND_TASKS.discard)

    return SpawnResponse(display_name=result.display_name, tmux_session=result.tmux_session)


@router.post("/kill-peer", response_model=KillResponse)
async def kill_registered_peer(
    request: KillPeerRequest,
    _: str | None = Depends(require_auth),
) -> KillResponse:
    """Kill a registered local peer by peer_id or display_name.

    Resolves mesh identity via PeerRegistry so callers do not need to know the
    tmux window name.
    """
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()
    await _authorize_kill(peer_registry, request.from_peer)
    resolved = await peer_registry.resolve_peer_strict(
        request.peer_identifier, circle=request.circle,
    )

    if isinstance(resolved, list):
        if not resolved:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail=f"Peer not found: {request.peer_identifier}",
            )
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": f"Ambiguous peer identifier: {request.peer_identifier}",
                "candidates": [
                    {
                        "peer_id": p.peer_id,
                        "display_name": p.display_name,
                        "circle": p.circle,
                        "tmux_session": p.tmux_session,
                    }
                    for p in resolved
                ],
            },
        )

    peer = resolved
    if peer.tmux_session:
        kill_peer(peer.tmux_session)
    await peer_registry.unregister_peer(peer.peer_id)
    return KillResponse()
