"""Spawn endpoints — create and kill agent sessions via tmux."""

from __future__ import annotations

import asyncio
import socket
from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import AgentType
from repowire.daemon.ask_tracker import QuiesceFailedError
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_config, get_peer_registry
from repowire.daemon.peer_registry import PeerRegistry
from repowire.installers.post_spawn import post_spawn_warmup
from repowire.spawn import (
    AGENT_COMMANDS,
    SpawnConfig,
    SpawnResult,
    kill_pane,
    spawn_peer,
)

_COMMAND_TO_BACKEND: dict[str, AgentType] = {
    cmd: backend for backend, cmd in AGENT_COMMANDS.items()
}

# Strong references to background warmup tasks. asyncio holds only weak refs to
# tasks, so without this set a long-sleeping warmup can be GC'd mid-flight.
_BACKGROUND_TASKS: set[asyncio.Task] = set()

# Pane ids of peers spawned via /spawn. Used by /kill-peer to gate tmux pane
# kills: only panes the daemon spawned should be killed. The OpenCode plugin
# sends tmux_session from any user-attached pane, so tmux_session alone is not
# a daemon-spawn signal.
#
# Lifecycle assumptions:
# - Lost on daemon restart → /kill-peer safe-fails to tmux_killed=None
# - Cleared on pane_died lifecycle event (see lifecycle_handler.handle_pane_died)
#   to prevent stale entries from matching a reused pane id after a tmux server
#   restart. Tmux pane ids are session-lifetime unique within a server, so this
#   set is only safe for the current tmux server's lifetime.
_SPAWNED_PANE_IDS: set[str] = set()


def forget_spawned_pane(pane_id: str) -> None:
    """Remove a pane id from the spawned set (called on pane_died lifecycle).

    Idempotent. Used by daemon.lifecycle_handler to prevent stale entries
    from matching reused pane ids after a tmux server restart.
    """
    _SPAWNED_PANE_IDS.discard(pane_id)


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
    """Result of a successful kill.

    `tmux_killed` reports whether the underlying tmux pane was terminated:
    - True  → pane kill attempted and succeeded
    - False → pane kill attempted but failed (likely orphan pane already gone;
              caller should verify with `tmux list-panes -a`)
    - None  → pane kill skipped because daemon ownership was not proven.
              Causes: peer was externally attached (no /spawn), peer had no
              pane_id, or daemon restarted since /spawn (in-memory ownership
              set was lost). Caller can verify with `tmux list-panes` and
              follow up with manual `tmux kill-pane` if needed.
    """

    ok: bool = True
    tmux_killed: bool | None = None


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

    # Record that we own this pane so /kill-peer can safely kill it later.
    # Externally-attached agents (notably OpenCode) also report tmux_session,
    # so tmux_session alone is NOT a daemon-spawn signal — pane_id ownership is.
    if result.pane_id:
        _SPAWNED_PANE_IDS.add(result.pane_id)

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
    # Only kill the pane if the daemon spawned it. We track spawned pane_ids
    # in _SPAWNED_PANE_IDS at /spawn time. tmux_session is NOT a reliable
    # ownership signal — the OpenCode plugin sends it from any user-attached
    # pane (installers/opencode.py:213-216), and any HTTP /peers caller could
    # too. Pane-id-set membership is the single source of truth.
    tmux_killed: bool | None = None
    if peer.pane_id and peer.pane_id in _SPAWNED_PANE_IDS:
        tmux_killed = kill_pane(peer.pane_id)
        _SPAWNED_PANE_IDS.discard(peer.pane_id)
    await peer_registry.unregister_peer(peer.peer_id)
    return KillResponse(tmux_killed=tmux_killed)


# ---------------------------------------------------------------------------
# Backend switcher (§4.8) — kill + respawn same path/circle with new backend
# ---------------------------------------------------------------------------


class SwitchBackendRequest(BaseModel):
    """Request to switch a running peer's backend."""

    new_backend: AgentType = Field(..., description="Target agent runtime")


class SwitchBackendResponse(BaseModel):
    """Result of a successful backend switch."""

    ok: bool = True
    display_name: str
    tmux_session: str
    old_backend: AgentType
    new_backend: AgentType
    command: str = Field(
        ..., description="The allowed_commands entry actually used to spawn",
    )


def _command_for_backend(new_backend: AgentType) -> str | None:
    """Return the first allowed_commands entry whose first token maps to new_backend."""
    cfg = get_config()
    for cmd in cfg.daemon.spawn.allowed_commands:
        head = cmd.split(None, 1)[0] if cmd else ""
        if _COMMAND_TO_BACKEND.get(head) is new_backend:
            return cmd
    return None


@router.post("/peers/{name}/switch-backend", response_model=SwitchBackendResponse)
async def switch_peer_backend(
    name: str,
    request: SwitchBackendRequest,
    circle: str | None = None,
    _: str | None = Depends(require_auth),
) -> SwitchBackendResponse:
    """Switch a peer's backend by killing it and respawning at the same path/circle.

    Same-host only in v1: cross-host peers return 409 (same shape as the per-peer
    MCP routes). Conversation state is NOT preserved across the switch — the new
    backend starts fresh in the peer's working directory. ACP-native switching is
    out of scope until ACP phase-3 lands.

    Race-safety:
    - In-flight asks are blocked atomically via AskTracker.begin_quiesce: the
      barrier both verifies no open asks exist AND prevents new /ask registrations
      targeting the peer (either direction) until the switch completes. New
      callers see 503 peer_switching during the window.
    - kill_pane failure aborts the switch (500 kill_failed) rather than
      unregistering a peer whose runtime is still alive.
    - Known limitation: between unregister_peer and the new agent's SessionStart
      self-register there is a small window where the peer name is absent from
      the registry. A concurrent /spawn for the same path+backend+circle could
      in theory race for the freed name. Operators triggering simultaneous
      switches against the same peer is the only path that hits this; addressing
      it requires a name-reservation primitive in PeerRegistry and is deferred.

    Errors:
    - 404: peer not found
    - 409 same_backend: new_backend equals current backend (no-op)
    - 409 cross_host: peer runs on a different machine
    - 409 in_flight_asks: peer has open asks; caller must retry after they close
    - 422 command_unavailable: no entry in daemon.spawn.allowed_commands maps to
      new_backend — operator must add one to ~/.repowire/config.yaml
    - 500 kill_failed: the daemon-owned tmux pane could not be killed; the old
      runtime may still be alive. Investigate with `tmux list-panes -a`.
    """
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()
    peer = await peer_registry.get_peer(name, circle=circle)
    if not peer:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

    self_machine = socket.gethostname()
    if peer.machine and peer.machine != self_machine:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "cross_host",
                "hint": (
                    "Backend switch is same-host only in v1; "
                    "ACP transport required for remote peers."
                ),
                "peer_machine": peer.machine,
                "self_machine": self_machine,
            },
        )

    current_backend = peer.backend
    if current_backend is request.new_backend:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "same_backend",
                "hint": f"Peer is already running {current_backend.value}",
                "backend": current_backend.value,
            },
        )

    if not peer.path:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "missing_path",
                "hint": "Peer has no recorded working directory; cannot respawn.",
            },
        )

    command = _command_for_backend(request.new_backend)
    if command is None:
        raise HTTPException(
            status_code=422,
            detail={
                "error": "command_unavailable",
                "hint": (
                    f"No entry in daemon.spawn.allowed_commands maps to "
                    f"{request.new_backend.value!r}. Add one to "
                    f"~/.repowire/config.yaml (e.g. {request.new_backend.value!r})."
                ),
                "new_backend": request.new_backend.value,
            },
        )

    # Validate the resolved command + peer's existing path against allowlists so
    # operators can't bypass /spawn's guardrails via a backend switch.
    _validate_spawn_request(peer.path, command)

    # Acquire the ask-tracker quiesce barrier atomically: this both verifies no
    # open asks exist for the peer AND blocks new /ask registrations targeting
    # the peer until end_quiesce() runs. Without the barrier a fresh /ask could
    # race between the pending check and the kill and be orphaned by the switch.
    state = get_app_state()
    ask_tracker = state.ask_tracker
    try:
        await ask_tracker.begin_quiesce(peer.peer_id)
    except QuiesceFailedError as e:
        if not e.open_cids:
            # Concurrent switch already holds the barrier — refuse so we don't
            # both enter the kill/spawn section and have the first end_quiesce
            # prematurely release the barrier for the second.
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "error": "switch_in_progress",
                    "hint": (
                        "Another switch is in progress for this peer. "
                        "Retry shortly."
                    ),
                },
            ) from e
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "in_flight_asks",
                "hint": (
                    "Peer has open asks; backend switch would orphan them. "
                    "Retry after they're acked or evicted."
                ),
                "open_asks": e.open_cids,
            },
        ) from e

    try:
        # Capture pre-kill state. Use the path-derived stem so the respawn lands
        # on the same window-name base (tmux will pick a unique suffix if
        # needed).
        spawn_circle = peer.circle or "default"
        resolved_path = str(Path(peer.path).expanduser().resolve())

        # Only kill daemon-owned panes (same ownership rule as /kill-peer). If
        # kill_pane returns False the underlying agent is still alive — abort
        # rather than leave a zombie runtime running against a deregistered
        # identity.
        if peer.pane_id and peer.pane_id in _SPAWNED_PANE_IDS:
            if not kill_pane(peer.pane_id):
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "error": "kill_failed",
                        "hint": (
                            "tmux kill-pane failed for the peer's pane; the "
                            "old agent may still be alive. Aborting switch to "
                            "avoid a zombie runtime. Check `tmux list-panes -a`."
                        ),
                        "pane_id": peer.pane_id,
                    },
                )
            _SPAWNED_PANE_IDS.discard(peer.pane_id)
        await peer_registry.unregister_peer(peer.peer_id)

        # Respawn with the new backend.
        try:
            result: SpawnResult = spawn_peer(
                SpawnConfig(
                    path=resolved_path,
                    circle=spawn_circle,
                    backend=request.new_backend,
                    command=command,
                )
            )
        except (ValueError, RuntimeError) as e:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail=str(e),
            ) from e

        if result.pane_id:
            _SPAWNED_PANE_IDS.add(result.pane_id)
            task = asyncio.create_task(
                post_spawn_warmup(
                    request.new_backend,
                    result.pane_id,
                    path=resolved_path,
                    circle=spawn_circle,
                    message=result.message,
                )
            )
            _BACKGROUND_TASKS.add(task)
            task.add_done_callback(_BACKGROUND_TASKS.discard)

        return SwitchBackendResponse(
            display_name=result.display_name,
            tmux_session=result.tmux_session,
            old_backend=current_backend,
            new_backend=request.new_backend,
            command=command,
        )
    finally:
        await ask_tracker.end_quiesce(peer.peer_id)
