"""Spawn endpoints — create and kill agent sessions via tmux."""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_config
from repowire.spawn import SpawnConfig, SpawnResult, kill_peer, spawn_peer

router = APIRouter(tags=["spawn"])


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


@router.post("/spawn", response_model=SpawnResponse)
async def spawn(
    request: SpawnRequest,
    _: str | None = Depends(require_auth),
) -> SpawnResponse:
    """Spawn a new agent coding session.

    The command must exactly match an entry in daemon.spawn.allowed_commands
    in ~/.repowire/config.yaml. If allowed_commands is empty, spawn is disabled.

    The spawned agent self-registers via its SessionStart hook once it starts.
    """
    cfg = get_config()
    allowed = cfg.daemon.spawn.allowed_commands

    if not allowed:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail=(
                "Spawn is disabled. Add daemon.spawn.allowed_commands"
                " to ~/.repowire/config.yaml"
            ),
        )
    if request.command not in allowed:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail=f"Command not in allowed_commands: {request.command!r}",
        )

    try:
        result: SpawnResult = spawn_peer(
            SpawnConfig(
                path=request.path,
                circle=request.circle,
                backend=AgentType.CLAUDE_CODE,  # informational only
                command=request.command,
            )
        )
    except (ValueError, RuntimeError) as e:
        raise HTTPException(status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail=str(e))

    return SpawnResponse(display_name=result.display_name, tmux_session=result.tmux_session)


@router.post("/kill", response_model=SpawnResponse)
async def kill(
    request: KillRequest,
    _: str | None = Depends(require_auth),
) -> dict:
    """Kill a spawned agent session by tmux_session reference."""
    ok = kill_peer(request.tmux_session)
    if not ok:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Session not found: {request.tmux_session}",
        )
    return {"ok": True}
