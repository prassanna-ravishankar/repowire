"""Spawn endpoints — create and kill agent sessions via tmux."""

from __future__ import annotations

from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_config
from repowire.spawn import SpawnConfig, SpawnResult, kill_peer, spawn_peer

router = APIRouter(tags=["spawn"])

# In-memory registry of tmux_sessions spawned by this daemon instance.
# Only sessions in this set can be killed via /kill.
_spawned_sessions: set[str] = set()


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


class SpawnResponse(BaseModel):
    """Result of a successful spawn."""

    ok: bool = True
    display_name: str
    tmux_session: str


class KillRequest(BaseModel):
    """Request to kill a spawned session."""

    tmux_session: str = Field(
        ..., description="Session ref returned by /spawn (e.g. 'default:myproject')"
    )


class KillResponse(BaseModel):
    """Result of a successful kill."""

    ok: bool = True


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

    try:
        result: SpawnResult = spawn_peer(
            SpawnConfig(
                path=str(Path(request.path).expanduser().resolve()),
                circle=request.circle,
                backend=AgentType.CLAUDE_CODE,  # informational only
                command=request.command,
            )
        )
    except (ValueError, RuntimeError) as e:
        raise HTTPException(status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail=str(e))

    _spawned_sessions.add(result.tmux_session)
    return SpawnResponse(display_name=result.display_name, tmux_session=result.tmux_session)


@router.post("/kill", response_model=KillResponse)
async def kill(
    request: KillRequest,
    _: str | None = Depends(require_auth),
) -> KillResponse:
    """Kill a spawned agent session.

    Only sessions previously spawned via /spawn on this daemon instance can be killed.
    """
    if request.tmux_session not in _spawned_sessions:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Session not found or not spawned by repowire: {request.tmux_session}",
        )

    ok = kill_peer(request.tmux_session)
    if not ok:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Tmux session not found: {request.tmux_session}",
        )

    _spawned_sessions.discard(request.tmux_session)
    return KillResponse()
