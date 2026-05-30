"""Spawn endpoints — create and kill agent sessions via tmux."""

from __future__ import annotations

import asyncio
import socket
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field, model_validator

from repowire.agent_backends import build_resume_command
from repowire.config.models import AgentType, SpawnProfile, apply_spawn_profile
from repowire.daemon.ask_tracker import QuiesceFailedError
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_config, get_peer_registry
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.resume_safety import resolve_resume_safety
from repowire.daemon.spawn_service import SpawnService
from repowire.hooks.utils import read_pane_runtime_metadata
from repowire.hooks.ws_hook_supervisor import maybe_respawn
from repowire.installers.post_spawn import post_spawn_warmup
from repowire.protocol.peers import Peer, PeerRole
from repowire.spawn import SpawnConfig, SpawnResult, kill_pane, spawn_peer
from repowire.spawn_ownership import (
    OwnershipValidation,
    _norm_path,
    find_spawn_ownership_for_peer,
    forget_spawn_ownership,
    probe_tmux_pane,
    record_spawn_ownership,
    validate_spawn_ownership,
)

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
    forget_spawn_ownership(pane_id)


def _coerce_backend(value: str) -> AgentType | None:
    try:
        return AgentType(value)
    except ValueError:
        return None


def _runtime_commands() -> dict[AgentType, str]:
    return get_config().daemon.spawn.commands


def _runtime_profiles() -> dict[AgentType, dict[str, SpawnProfile]]:
    return get_config().daemon.spawn.profiles


def _resolve_spawn_command(
    *,
    backend: AgentType | None = None,
    legacy_command: str | None = None,
    profile: str | None = None,
) -> tuple[AgentType, str]:
    """Resolve a runtime profile to its launch command.

    ``backend`` is the preferred path. ``legacy_command`` is accepted for one
    compatibility release: if it matches a backend/profile name, resolve that;
    otherwise it may match a configured command value from /spawn/config.
    """
    commands = _runtime_commands()
    profiles = _runtime_profiles()
    if backend is not None:
        command = commands.get(backend)
        if command:
            if profile:
                selected_profile = profiles.get(backend, {}).get(profile)
                if selected_profile is None:
                    raise HTTPException(
                        status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
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
            return backend, command
        raise HTTPException(
            status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail={
                "error": "command_unavailable",
                "hint": (
                    f"No daemon.spawn.commands entry for {backend.value!r}. "
                    "Add it to ~/.repowire/config.yaml."
                ),
                "backend": backend.value,
            },
        )

    if legacy_command:
        if profile:
            raise HTTPException(
                status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
                detail="Pass backend with profile; command is a compatibility selector.",
            )
        alias_backend = _coerce_backend(legacy_command)
        if alias_backend is not None and alias_backend in commands:
            return alias_backend, commands[alias_backend]

        for candidate_backend, candidate_command in commands.items():
            if legacy_command == candidate_command:
                return candidate_backend, candidate_command

        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={
                "error": "command_unavailable",
                "hint": (
                    f"Command/profile not configured: {legacy_command!r}. "
                    "Use daemon.spawn.commands keyed by backend."
                ),
                "command": legacy_command,
            },
        )

    raise HTTPException(
        status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
        detail="Either backend or legacy command is required.",
    )


router = APIRouter(tags=["spawn"])


class SpawnConfigResponse(BaseModel):
    """Spawn configuration for UI discovery."""

    enabled: bool
    commands: dict[AgentType, str] = Field(default_factory=dict)
    profiles: dict[AgentType, dict[str, SpawnProfile]] = Field(default_factory=dict)
    allowed_commands: list[str] = Field(default_factory=list)
    allowed_paths: list[str] = Field(default_factory=list)


@router.get("/spawn/config", response_model=SpawnConfigResponse)
async def get_spawn_config(
    _: str | None = Depends(require_auth),
) -> SpawnConfigResponse:
    """Return spawn configuration so the UI can offer spawn controls."""
    cfg = get_config()
    cmds = cfg.daemon.spawn.commands
    profiles = cfg.daemon.spawn.profiles
    paths = cfg.daemon.spawn.allowed_paths
    return SpawnConfigResponse(
        enabled=bool(cmds and paths),
        commands=cmds,
        profiles=profiles,
        allowed_commands=list(cmds.values()),
        allowed_paths=paths,
    )


class SpawnRequest(BaseModel):
    """Request to spawn a new agent session."""

    path: str = Field(..., description="Absolute path to the project directory")
    backend: AgentType | None = Field(None, description="Backend/runtime profile to spawn")
    profile: str | None = Field(None, description="Optional named model/profile for backend")
    command: str | None = Field(
        None,
        description="Deprecated: command/profile name for compatibility",
    )
    circle: str = Field(default="default", description="Circle to spawn into")
    message: str | None = Field(
        default=None,
        description=(
            "Optional warmup message to send to the spawned agent. Used by "
            "backends whose hook lifecycle requires a first prompt (codex). "
            "Other backends ignore it. Default: a short branded warmup."
        ),
    )
    role: PeerRole = Field(default=PeerRole.AGENT, description="Peer role to seed")

    @model_validator(mode="after")
    def _single_runtime_selector(self) -> SpawnRequest:
        if self.backend is not None and self.command is not None:
            raise ValueError("Pass backend or command, not both")
        if self.backend is None and self.command is None:
            raise ValueError("Pass backend or command")
        if self.profile is not None and self.backend is None:
            raise ValueError("Pass backend with profile")
        return self


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


def _record_daemon_spawn_ownership(
    result: SpawnResult,
    *,
    path: str,
    backend: AgentType,
    circle: str,
    role: PeerRole | str,
    peer_id: str | None = None,
) -> None:
    """Record explicit ownership for panes created by daemon spawn/restart."""

    if not result.pane_id:
        return
    record_spawn_ownership(
        pane_id=result.pane_id,
        path=path,
        backend=backend,
        circle=circle,
        role=role,
        display_name=result.display_name,
        tmux_session=result.tmux_session,
        peer_id=peer_id,
    )


def _validate_spawn_path(path: str) -> None:
    """Validate path against configured spawn roots.

    Raises HTTPException 403 if spawn is disabled or the path is not allowed.
    Raises HTTPException 404 if the path does not exist on disk.
    """
    cfg = get_config()
    allowed_paths = cfg.daemon.spawn.allowed_paths
    commands = cfg.daemon.spawn.commands

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

    The backend command and path must be configured in daemon.spawn.commands /
    allowed_paths in ~/.repowire/config.yaml.
    The spawned agent self-registers via its SessionStart hook once it starts.
    """
    if request.backend is None:
        backend, _command = _resolve_spawn_command(
            backend=request.backend,
            legacy_command=request.command,
            profile=request.profile,
        )
    else:
        backend = request.backend
    service = getattr(get_app_state(), "spawn_service", None) or SpawnService(
        config=get_config(),
        spawned_pane_ids=_SPAWNED_PANE_IDS,
        background_tasks=_BACKGROUND_TASKS,
        spawn_impl=spawn_peer,
        warmup_impl=post_spawn_warmup,
    )
    result = service.spawn(
        path=request.path,
        backend=backend,
        profile=request.profile,
        circle=request.circle,
        message=request.message,
        role=request.role,
    )
    return SpawnResponse(display_name=result.display_name, tmux_session=result.tmux_session)


def _durable_ownership_error_detail(peer: Peer) -> dict[str, object]:
    validation = _effective_ownership_validation(peer)
    return {
        "error": validation.error or "unsupported_pane_ownership",
        "hint": validation.hint
        or (
            "Restart only supports panes Repowire can prove it spawned. "
            "Externally attached peers are left untouched."
        ),
        "pane_id": peer.pane_id,
    }


def _has_spawn_ownership(peer: Peer) -> bool:
    """Return whether Repowire can prove it spawned this peer's pane."""

    if peer.pane_id and peer.pane_id in _SPAWNED_PANE_IDS:
        return True
    return _effective_ownership_validation(peer).ok


def _effective_ownership_validation(peer: Peer) -> OwnershipValidation:
    """Validate explicit ownership, falling back to unique identity proof.

    After a daemon restart, a rehydrated peer can have no pane id or a stale pane
    id from old runtime metadata. A direct valid pane proof wins, but failed
    direct validation may fall back to a unique durable proof for the exact peer
    identity plus live tmux evidence.
    """

    direct: OwnershipValidation | None = None
    if peer.pane_id:
        direct = validate_spawn_ownership(peer)
        if direct.ok:
            return direct
    adopted = find_spawn_ownership_for_peer(peer)
    if adopted.ok or direct is None or adopted.error == "ambiguous_ownership":
        return adopted
    return direct


def _peer_with_adopted_ownership(peer: Peer) -> Peer:
    """Return a peer copy with durable pane proof adopted when uniquely valid."""

    validation = _effective_ownership_validation(peer)
    if not validation.ok or validation.record is None:
        return peer
    if peer.pane_id == validation.record.pane_id:
        return peer
    return peer.model_copy(
        update={
            "pane_id": validation.record.pane_id,
            "tmux_session": validation.record.tmux_session,
            "machine": validation.record.machine,
        }
    )


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
        request.peer_identifier,
        circle=request.circle,
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

    peer = _peer_with_adopted_ownership(resolved)
    # Only kill the pane if the daemon spawned it. We track spawned pane_ids
    # in _SPAWNED_PANE_IDS at /spawn time. tmux_session is NOT a reliable
    # ownership signal — the OpenCode plugin sends it from any user-attached
    # pane (installers/opencode.py:213-216), and any HTTP /peers caller could
    # too. Pane-id-set membership is the single source of truth.
    tmux_killed: bool | None = None
    if _has_spawn_ownership(peer):
        pane_id = peer.pane_id
        assert pane_id is not None
        tmux_killed = kill_pane(pane_id)
        forget_spawned_pane(pane_id)
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
    command: str = Field(..., description="The configured backend command used to spawn")


class RestartPeerRequest(BaseModel):
    """Request to intentionally restart a peer on the same backend."""

    circle: str | None = Field(None, description="Circle to scope display-name lookup")
    from_peer: str | None = Field(
        None, description="Caller peer_id or display_name — used for future authorization"
    )
    dry_run: bool = Field(False, description="Validate restart capability without killing")
    message: str | None = Field(
        None,
        description=(
            "Optional seed message delivered to the restarted runtime after boot. "
            "Does not imply backend transcript resume."
        ),
    )


class RestartPeerResponse(BaseModel):
    """Result of a peer restart request."""

    ok: bool = True
    status: str
    restarted: bool = False
    peer_id: str
    display_name: str
    backend: AgentType
    path: str
    circle: str
    tmux_session: str | None = None
    resume_mode: str = "fresh_runtime_context"
    resume_warning: str | None = None
    unsupported_reason: str | None = None
    command: str | None = None


def _command_for_backend(new_backend: AgentType) -> str | None:
    """Return the configured command for a backend/runtime profile."""
    return _runtime_commands().get(new_backend)


@dataclass(frozen=True)
class _RestartResume:
    """Resolved resume decision for a restart."""

    command: str          # base command, or resume command when resumable
    mode: str             # "resumed" | "fresh_runtime_context"
    resumable: bool       # a valid on-disk session was found and resume built
    warning: str | None   # why context will NOT be resumed (when not resumable)


def _resolve_restart_resume(
    *,
    peer: Peer,
    resolved_path: str,
    base_command: str,
    state: object,
) -> _RestartResume:
    """Decide whether a restart can resume the backend's prior session.

    Reads the peer's newest matching session binding for a runtime_session_id,
    pre-validates that the backend session file exists on disk (both Claude and
    Codex exit hard on an unknown id -- no fresh fallback), and builds the
    backend resume command in the SAME cwd. Falls back to a fresh respawn with
    an explicit warning when no resumable session is available.
    """
    store = getattr(state, "session_binding_store", None)
    chosen_id: str | None = None
    chosen_binding: Any = None
    if store is not None:
        # Newest binding for this peer, filtered to same backend + project path so
        # stale cross-path/identity-reuse state isn't selected (codex review B).
        backend_value = peer.backend.value
        for binding in store.list_by_peer(peer.peer_id):  # ORDER BY last_seen_at DESC
            if binding.runtime_session_id is None:
                continue
            if binding.backend and binding.backend != backend_value:
                continue
            if binding.project_path and _norm_path(binding.project_path) != _norm_path(
                resolved_path
            ):
                continue
            chosen_id = binding.runtime_session_id
            chosen_binding = binding
            break

    decision = resolve_resume_safety(
        backend=peer.backend,
        path=resolved_path,
        runtime_session_id=chosen_id,
        repowire_session_id=chosen_binding.repowire_session_id if chosen_binding else None,
        # Honor a binding that recorded resume as unsupported, same as the job
        # and session-control pathways (codex review item 3).
        capability=chosen_binding.resume_capability if chosen_binding else None,
    )
    if not decision.resumable or decision.plan is None:
        return _RestartResume(base_command, "fresh_runtime_context", False, decision.warning)
    resume_cmd = build_resume_command(base_command, decision.plan)
    return _RestartResume(resume_cmd, "resumed", True, None)


@router.post("/peers/{name}/restart", response_model=RestartPeerResponse)
async def restart_peer(
    name: str,
    request: RestartPeerRequest,
    _: str | None = Depends(require_auth),
) -> RestartPeerResponse:
    """Restart a daemon-spawned peer while preserving mesh identity.

    This is intentionally narrower than transcript resume. It kills a
    daemon-owned local pane, respawns the same backend/path/circle, and passes
    the existing peer_id through the spawn hint so the new SessionStart can
    reconnect to the same mesh identity. Backends reload their normal runtime
    context on startup; exact conversation resume is backend-specific and not
    claimed here.
    """
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()
    await _authorize_kill(peer_registry, request.from_peer)
    resolved = await peer_registry.resolve_peer_strict(name, circle=request.circle)

    if isinstance(resolved, list):
        if not resolved:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail=f"Peer not found: {name}",
            )
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": f"Ambiguous peer identifier: {name}",
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

    peer = _peer_with_adopted_ownership(resolved)
    self_machine = socket.gethostname()
    if peer.machine and peer.machine != "unknown" and peer.machine != self_machine:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "cross_host",
                "hint": "Peer restart is same-host only in this slice.",
                "peer_machine": peer.machine,
                "self_machine": self_machine,
            },
        )

    if not peer.path:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "missing_path",
                "hint": "Peer has no recorded working directory; cannot restart.",
            },
        )

    command = _command_for_backend(peer.backend)
    if command is None:
        raise HTTPException(
            status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail={
                "error": "command_unavailable",
                "hint": (
                    f"No entry in daemon.spawn.commands maps to {peer.backend.value!r}. "
                    f"Add daemon.spawn.commands.{peer.backend.value} to ~/.repowire/config.yaml."
                ),
                "backend": peer.backend.value,
            },
        )

    _validate_spawn_path(peer.path)

    spawn_circle = peer.circle or "default"
    resolved_path = str(Path(peer.path).expanduser().resolve())

    # Resolve resume BEFORE the ownership refusal so the refusal can hand back a
    # manual resume command (codex review A). Both Claude and Codex exit hard on
    # an unknown session id (no fresh fallback), so we pre-validate the id is on
    # disk before committing to a resume that would kill the pane (codex C).
    resume_resolution = _resolve_restart_resume(
        peer=peer, resolved_path=resolved_path, base_command=command, state=get_app_state(),
    )
    resume_command = resume_resolution.command
    resume_mode = resume_resolution.mode
    resume_warning = resume_resolution.warning

    if not _has_spawn_ownership(peer):
        detail = _durable_ownership_error_detail(peer)
        detail["error"] = "unsupported_pane_ownership"
        if resume_resolution.resumable:
            # We won't kill a pane we can't prove we own, but the peer IS
            # resumable -- give the user the exact command to relaunch with
            # context manually.
            detail["resume_command"] = resume_command
            detail["resume_mode"] = "manual_resume_available"
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=detail,
        )

    if request.dry_run:
        return RestartPeerResponse(
            status="restart_available",
            restarted=False,
            peer_id=peer.peer_id,
            display_name=peer.display_name,
            backend=peer.backend,
            path=resolved_path,
            circle=spawn_circle,
            tmux_session=peer.tmux_session,
            command=resume_command,
            resume_mode=resume_mode,
            resume_warning=resume_warning,
        )

    state = get_app_state()
    ask_tracker = state.ask_tracker
    try:
        await ask_tracker.begin_quiesce(peer.peer_id)
    except QuiesceFailedError as e:
        if not e.open_cids:
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "error": "restart_in_progress",
                    "hint": "Another restart/switch is in progress for this peer. Retry shortly.",
                },
            ) from e
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "in_flight_asks",
                "hint": (
                    "Peer has open asks; restart would orphan them. "
                    "Retry after they're acked or evicted."
                ),
                "open_asks": e.open_cids,
            },
        ) from e

    try:
        pane_id = peer.pane_id
        if not _has_spawn_ownership(peer):
            detail = _durable_ownership_error_detail(peer)
            detail["error"] = "unsupported_pane_ownership"
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail=detail,
            )
        assert pane_id is not None
        if not kill_pane(pane_id):
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "error": "kill_failed",
                    "hint": (
                        "tmux kill-pane failed for the peer's pane; the old runtime may "
                        "still be alive. Aborting restart to avoid duplicate runtimes."
                    ),
                    "pane_id": pane_id,
                },
            )
        forget_spawned_pane(pane_id)
        await peer_registry.mark_offline(peer.peer_id)

        try:
            result: SpawnResult = spawn_peer(
                SpawnConfig(
                    path=resolved_path,
                    circle=spawn_circle,
                    backend=peer.backend,
                    command=resume_command,
                    message=request.message,
                    role=peer.role.value,
                    peer_id=peer.peer_id,
                )
            )
        except (ValueError, RuntimeError) as e:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail=str(e),
            ) from e

        if result.pane_id:
            _SPAWNED_PANE_IDS.add(result.pane_id)
            _record_daemon_spawn_ownership(
                result,
                path=resolved_path,
                backend=peer.backend,
                circle=spawn_circle,
                role=peer.role,
                peer_id=peer.peer_id,
            )
            task = asyncio.create_task(
                post_spawn_warmup(
                    peer.backend,
                    result.pane_id,
                    path=resolved_path,
                    circle=spawn_circle,
                    message=result.message,
                )
            )
            _BACKGROUND_TASKS.add(task)
            task.add_done_callback(_BACKGROUND_TASKS.discard)

        return RestartPeerResponse(
            status="restarted",
            restarted=True,
            peer_id=peer.peer_id,
            display_name=peer.display_name,
            backend=peer.backend,
            path=resolved_path,
            circle=spawn_circle,
            tmux_session=result.tmux_session,
            command=resume_command,
            resume_mode=resume_mode,
            resume_warning=resume_warning,
        )
    finally:
        await ask_tracker.end_quiesce(peer.peer_id)


class RehookPeerRequest(BaseModel):
    """Request to re-establish a peer's inbound ws-hook without killing the pane."""

    circle: str | None = Field(None, description="Circle to scope display-name lookup")
    from_peer: str | None = Field(
        None, description="Caller peer_id or display_name — used for authorization"
    )
    apply: bool = Field(
        False,
        description=(
            "Act instead of dry-run. Default False (report-only) because this "
            "touches a live pane's hook even though it never kills the pane."
        ),
    )


class RehookPeerResponse(BaseModel):
    """Result of a peer rehook request."""

    ok: bool = True
    acted: bool = False
    peer_id: str
    display_name: str
    pane_id: str | None = None
    ws_was_connected: bool = False
    ping_ok: bool | None = None
    pane_verified: bool = False
    ws_hook_respawned: bool = False
    reason: str


@router.post("/peers/{name}/rehook", response_model=RehookPeerResponse)
async def rehook_peer(
    name: str,
    request: RehookPeerRequest,
    _: str | None = Depends(require_auth),
) -> RehookPeerResponse:
    """Re-establish a peer's inbound ws-hook WITHOUT killing the pane or agent.

    Non-destructive recovery for the "registered/online but inbound is dead"
    case. Same-host only (the daemon cannot spawn a ws-hook into a foreign
    pane). Default is dry-run; pass ``apply=true`` to act. A ping-healthy
    connection is left untouched — rehook never disconnects a working peer.
    Destructive restart stays the separate ``/peers/{name}/restart``.
    """
    peer_registry = get_peer_registry()
    state = get_app_state()
    transport = state.transport
    await peer_registry.lazy_repair()
    await _authorize_kill(peer_registry, request.from_peer)
    resolved = await peer_registry.resolve_peer_strict(name, circle=request.circle)

    if isinstance(resolved, list):
        if not resolved:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail=f"Peer not found: {name}",
            )
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={"error": f"Ambiguous peer identifier: {name}"},
        )

    peer = _peer_with_adopted_ownership(resolved)
    self_machine = socket.gethostname()
    if peer.machine and peer.machine != "unknown" and peer.machine != self_machine:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "cross_host",
                "hint": "Peer rehook is same-host only.",
                "peer_machine": peer.machine,
                "self_machine": self_machine,
            },
        )

    if not peer.pane_id:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "missing_pane",
                "hint": "Peer has no recorded pane; nothing to rehook.",
            },
        )

    # Ownership gate: prove the pane is real AND belongs to THIS peer before
    # starting a hook that will inject future messages into it. Pane-exists
    # alone is insufficient — a stale/reused pane_id would misroute. Accept:
    #   (a) durable Repowire spawn-ownership proof, or
    #   (b) live tmux evidence whose current_path matches the peer's path, or
    #   (c) pane hook metadata whose peer_id/display_name matches this peer.
    pane_verified = _has_spawn_ownership(peer)
    if not pane_verified:
        evidence = await asyncio.to_thread(probe_tmux_pane, peer.pane_id)
        if evidence is not None and peer.path and _norm_path(evidence.current_path) == _norm_path(
            peer.path
        ):
            pane_verified = True
        else:
            meta = read_pane_runtime_metadata(peer.pane_id)
            if meta.get("peer_id") == peer.peer_id or (
                meta.get("display_name")
                and meta.get("display_name") == peer.display_name
            ):
                pane_verified = True
    if not pane_verified:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "pane_unverified",
                "hint": (
                    "Could not prove the pane belongs to this peer "
                    "(no spawn ownership, pane path mismatch, and hook metadata mismatch)."
                ),
                "pane_id": peer.pane_id,
            },
        )

    # Diagnose the connection before touching it: a ping-healthy socket means
    # the hook is fine and must NOT be disconnected.
    ws_was_connected = transport.is_connected(peer.peer_id)
    ping_ok: bool | None = None
    if ws_was_connected:
        try:
            pong = await transport.ping(peer.peer_id, timeout=2.0)
            ping_ok = bool(pong)
        except Exception:
            ping_ok = False
        if ping_ok:
            return RehookPeerResponse(
                acted=False,
                peer_id=peer.peer_id,
                display_name=peer.display_name,
                pane_id=peer.pane_id,
                ws_was_connected=True,
                ping_ok=True,
                pane_verified=True,
                ws_hook_respawned=False,
                reason="already_healthy",
            )

    if not request.apply:
        return RehookPeerResponse(
            acted=False,
            peer_id=peer.peer_id,
            display_name=peer.display_name,
            pane_id=peer.pane_id,
            ws_was_connected=ws_was_connected,
            ping_ok=ping_ok,
            pane_verified=True,
            ws_hook_respawned=False,
            reason="dry_run",
        )

    # Act: drop only a confirmed-stale connection, then attempt a respawn.
    if ws_was_connected and ping_ok is False:
        await transport.disconnect(peer.peer_id)

    respawned = await asyncio.to_thread(
        maybe_respawn,
        peer.pane_id,
        backend=peer.backend.value,
        cwd=peer.path or None,
    )
    # maybe_respawn no-ops when the pid file still reads alive even though the
    # WS is dead (its liveness check is pid-based). Report that honestly.
    reason = "respawned" if respawned else "respawn_skipped_pid_alive_or_contested"
    return RehookPeerResponse(
        acted=True,
        peer_id=peer.peer_id,
        display_name=peer.display_name,
        pane_id=peer.pane_id,
        ws_was_connected=ws_was_connected,
        ping_ok=ping_ok,
        pane_verified=True,
        ws_hook_respawned=respawned,
        reason=reason,
    )


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
    - 422 command_unavailable: no entry in daemon.spawn.commands maps to
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
                    f"No entry in daemon.spawn.commands maps to "
                    f"{request.new_backend.value!r}. Add "
                    f"daemon.spawn.commands.{request.new_backend.value} to "
                    "~/.repowire/config.yaml."
                ),
                "new_backend": request.new_backend.value,
            },
        )

    # Validate the resolved command + peer's existing path against config so
    # operators can't bypass /spawn's guardrails via a backend switch.
    _validate_spawn_path(peer.path)

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
                    "hint": ("Another switch is in progress for this peer. Retry shortly."),
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
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail=str(e),
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
