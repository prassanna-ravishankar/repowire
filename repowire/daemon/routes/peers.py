"""Peer management endpoints."""

from __future__ import annotations

import asyncio
import logging
import os
import socket
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Query, status
from pydantic import BaseModel, Field, field_validator

from repowire import peer_mcp
from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.peer_registry import (
    CircleSource,
    PaneHijackRejectedError,
    RoleClaimConflictError,
)
from repowire.daemon.routes._shared import OkResponse, is_valid_identifier
from repowire.daemon.state.session_bindings import SessionBinding
from repowire.protocol.peers import Peer, PeerRole, PeerStatus, TurnState
from repowire.session.history import (
    HistoryLoadResult,
    load_bound_history,
    load_peer_history,
    page_turns,
)
from repowire.session.timeline import TimelineItem, build_session_timeline

router = APIRouter(tags=["peers"])
logger = logging.getLogger(__name__)


def _metadata_source_uri(metadata: dict[str, Any] | None) -> str | None:
    if not metadata:
        return None
    for key in ("runtime_source_uri", "source_uri", "transcript_source_uri"):
        value = metadata.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def _binding_metadata(metadata: dict[str, Any] | None) -> dict[str, Any]:
    if not metadata:
        return {}
    allowed_keys = (
        "hook_session_id",
        "runtime_source_uri",
        "source_uri",
        "transcript_source_uri",
    )
    return {key: metadata[key] for key in allowed_keys if metadata.get(key) is not None}


class PeerInfo(BaseModel):
    """Peer information for API responses."""

    peer_id: str
    name: str  # Backward compat (= display_name)
    display_name: str
    path: str | None = None
    machine: str | None = None
    tmux_session: str | None = None
    backend: str = "claude-code"
    circle: str = "global"
    role: PeerRole = PeerRole.AGENT
    status: str
    turn_state: TurnState | None = None
    last_seen: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    description: str = ""


def _peer_to_info(p: Peer) -> PeerInfo:
    """Convert a Peer model to a PeerInfo API response."""
    return PeerInfo(
        peer_id=p.peer_id,
        name=p.display_name,
        display_name=p.display_name,
        path=p.path,
        machine=p.machine,
        tmux_session=p.tmux_session,
        backend=p.backend,
        circle=p.circle,
        role=p.role,
        status=p.status.value,
        turn_state=p.turn_state,
        last_seen=p.last_seen.isoformat() if p.last_seen else None,
        metadata=p.metadata,
        description=p.description,
    )


class PeersResponse(BaseModel):
    """Response containing list of peers."""

    peers: list[PeerInfo]


class ClaimRoleRequest(BaseModel):
    """Request to claim a singleton special role for an existing peer."""

    role: PeerRole = Field(..., description="Special role to claim")
    peer_name: str = Field(..., min_length=1, description="Existing peer id or display name")
    circle: str | None = Field(None, description="Circle to scope lookup/claim")
    force: bool = Field(False, description="Demote an existing live holder")


class ClaimRoleResponse(BaseModel):
    """Response from a successful special role claim."""

    ok: bool = True
    peer_id: str
    peer_name: str
    role: PeerRole
    circle: str
    already_held: bool = False
    previous_holders: list[dict[str, str | None]] = Field(default_factory=list)


class RegisterPeerRequest(BaseModel):
    """Request to register a peer."""

    name: str = Field(..., min_length=1, pattern=r"^[a-zA-Z0-9._-]+$", description="Peer name")
    path: str | None = Field(None, description="Working directory path")
    machine: str | None = Field(None, description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session:window")
    pane_id: str | None = Field(None, description="Tmux pane ID")
    backend: AgentType = Field(default=AgentType.CLAUDE_CODE, description="Agent type")
    circle: str | None = Field(None, description="Circle (logical subnet)")
    circle_source: CircleSource | None = Field(
        None,
        description="How the caller resolved circle: tmux, spawn_hint, or fallback",
    )
    role: PeerRole = Field(default=PeerRole.AGENT, description="Peer role")
    turn_state: TurnState | None = Field(
        None, description="Initial per-turn progress (optional)"
    )
    agent_pid: int | None = Field(
        None,
        description=(
            "PID of the agent process that owns this peer "
            "(os.getppid() from the registering hook, i.e. the hook's "
            "parent, NOT the hook's own pid)."
        ),
    )
    parent_pid: int | None = Field(
        None,
        description=(
            "Parent PID of the registering hook (os.getppid()). "
            "Used for pane-hijack detection."
        ),
    )
    metadata: dict[str, Any] = Field(default_factory=dict)

    @field_validator("circle")
    @classmethod
    def validate_circle(cls, v: str | None) -> str | None:
        if v is not None and not is_valid_identifier(v):
            raise ValueError("Circle must match ^[a-zA-Z0-9._-]+$ and be <= 64 chars")
        return v


class UnregisterPeerRequest(BaseModel):
    """Request to unregister a peer."""

    name: str = Field(..., description="Peer name to unregister")



def _resolve_realpath_map(paths: list[str]) -> dict[str, str]:
    """Resolve realpath for each unique path. Blocking I/O - run in thread."""
    return {p: os.path.realpath(p) for p in set(paths)}


@router.get("/peers", response_model=PeersResponse)
async def list_peers(
    status: str | None = Query(None, description="Filter by status", enum=["online", "offline"]),
    path: str | None = Query(None, description="Filter by absolute path"),
    backend: AgentType | None = Query(None, description="Filter by backend"),
    circle: str | None = Query(
        None,
        description=(
            "Filter by circle. Omit or '*' for mesh-wide. A concrete name returns peers "
            "in that circle plus any peer whose role bypasses circles (service, "
            "orchestrator, human)."
        ),
    ),
    _: str | None = Depends(require_auth),
) -> PeersResponse:
    """Get list of all registered peers, optionally filtered."""
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()
    peers = await peer_registry.get_all_peers()

    if status == "online":
        peers = [p for p in peers if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)]
    elif status == "offline":
        peers = [p for p in peers if p.status == PeerStatus.OFFLINE]
    if path:
        # Normalize symlinks so registrations using different path forms still match.
        # realpath is blocking I/O; resolve every unique path once in a thread.
        string_matches = [p for p in peers if p.path == path]
        mismatched = [p for p in peers if p.path and p.path != path]
        target, resolved_map = await asyncio.gather(
            asyncio.to_thread(os.path.realpath, path),
            asyncio.to_thread(_resolve_realpath_map, [p.path for p in mismatched]),
        )
        peers = string_matches + [p for p in mismatched if resolved_map.get(p.path) == target]
    if backend:
        peers = [p for p in peers if p.backend == backend]
    if circle is not None and circle != "*":
        peers = [p for p in peers if p.circle == circle or p.bypasses_circles]

    return PeersResponse(peers=[_peer_to_info(p) for p in peers])


@router.get("/peers/by-pane/{pane_id}", response_model=PeerInfo)
async def get_peer_by_pane(
    pane_id: str,
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get peer by tmux pane ID."""
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer_by_pane(pane_id)
    if peer:
        return _peer_to_info(peer)
    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail=f"No peer for pane: {pane_id}",
    )


@router.post(
    "/peers/claim-role",
    response_model=ClaimRoleResponse,
    include_in_schema=False,
)
async def claim_peer_role(
    request: ClaimRoleRequest,
    _: str | None = Depends(require_auth),
) -> ClaimRoleResponse:
    """Claim a singleton special role for an existing peer.

    CLI repair surface only in v0.13. This route is intentionally not exposed
    through MCP/Pi tools.
    """
    if request.role != PeerRole.ORCHESTRATOR:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="Only role=orchestrator can be claimed",
        )
    peer_registry = get_peer_registry()
    try:
        result = await peer_registry.claim_special_role(
            request.peer_name,
            request.role,
            circle=request.circle,
            force=request.force,
        )
    except RoleClaimConflictError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=str(exc),
        ) from exc
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail=str(exc),
        ) from exc
    if result is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {request.peer_name}",
        )
    return ClaimRoleResponse(
        peer_id=result.peer.peer_id,
        peer_name=result.peer.display_name,
        role=result.peer.role,
        circle=result.peer.circle,
        already_held=result.already_held,
        previous_holders=result.previous_holders,
    )


@router.get("/peers/{identifier}", response_model=PeerInfo)
async def get_peer(
    identifier: str,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get information about a specific peer by peer_id or display_name."""
    peer_registry = get_peer_registry()
    try:
        peer = await peer_registry.get_peer(identifier, circle=circle)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=str(exc),
        ) from exc
    if peer:
        return _peer_to_info(peer)

    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail=f"Peer not found: {identifier}",
    )


class RegisterResponse(BaseModel):
    """Response from peer registration with daemon-assigned identity."""

    ok: bool = True
    peer_id: str
    display_name: str


async def _register_peer_impl(request: RegisterPeerRequest) -> tuple[str, str]:
    """Shared implementation for peer registration endpoints.

    Returns (peer_id, assigned_display_name).
    """
    circle = request.circle or "global"

    peer_registry = get_peer_registry()
    try:
        peer_id, display_name = await peer_registry.allocate_and_register(
            circle=circle,
            backend=request.backend,
            path=request.path or "",
            pane_id=request.pane_id,
            tmux_session=request.tmux_session,
            metadata=request.metadata,
            machine=request.machine or socket.gethostname(),
            role=request.role,
            turn_state=request.turn_state,
            initial_status=(
                PeerStatus.OFFLINE
                if request.pane_id and request.metadata.get("hook_session_id")
                else PeerStatus.ONLINE
            ),
            agent_pid=request.agent_pid,
            parent_pid=request.parent_pid,
            circle_source=request.circle_source,
        )
    except PaneHijackRejectedError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=str(exc),
        ) from exc
    binding_store = getattr(get_app_state(), "session_binding_store", None)
    if binding_store is not None:
        try:
            binding_store.upsert_observation(
                peer_id=peer_id,
                backend=request.backend,
                project_path=request.path,
                runtime_session_id=request.metadata.get("hook_session_id"),
                runtime_source_uri=_metadata_source_uri(request.metadata),
                provenance={
                    "source_kind": "runtime_hook",
                    "backend": request.backend.value,
                    "runtime_session_id": request.metadata.get("hook_session_id"),
                    "observed_by_peer_id": peer_id,
                },
                status="active",
                metadata=_binding_metadata(request.metadata),
            )
        except Exception:
            logger.warning("Failed to persist session binding for peer registration", exc_info=True)
    return peer_id, display_name


@router.post("/peers", response_model=RegisterResponse)
async def create_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> RegisterResponse:
    """Register a new peer (CLI-friendly endpoint)."""
    peer_id, display_name = await _register_peer_impl(request)
    return RegisterResponse(peer_id=peer_id, display_name=display_name)


async def _unregister_peer_impl(name: str, circle: str | None = None) -> None:
    """Shared unregister logic: remove from PeerRegistry."""
    peer_registry = get_peer_registry()

    peer = await peer_registry.get_peer(name, circle=circle)
    if not peer:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

    await peer_registry.unregister_peer(name, circle=circle)


@router.delete("/peers/{name}", response_model=OkResponse)
async def delete_peer(
    name: str,
    circle: str | None = Query(None, description="Circle to scope deletion to avoid ambiguity"),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer by name (CLI-friendly endpoint)."""
    await _unregister_peer_impl(name, circle=circle)
    return OkResponse()


class OfflineResponse(BaseModel):
    """Response for marking peer offline."""

    ok: bool = True
    cancelled_queries: int = 0


@router.post("/peers/{name}/offline", response_model=OfflineResponse)
async def mark_peer_offline(
    name: str,
    _: str | None = Depends(require_auth),
) -> OfflineResponse:
    """Mark a peer as offline and cancel pending queries to it.

    Called by SessionEnd hook when a Claude session closes.
    """
    peer_registry = get_peer_registry()
    cancelled = await peer_registry.mark_offline(name)
    return OfflineResponse(cancelled_queries=cancelled)


class SetDescriptionRequest(BaseModel):
    """Request to set peer's description."""

    description: str = Field(..., description="Current task description")


@router.post("/peers/{name}/description", response_model=OkResponse)
async def set_peer_description(
    name: str,
    request: SetDescriptionRequest,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Update a peer's task description."""
    peer_registry = get_peer_registry()
    try:
        found = await peer_registry.update_description(name, request.description, circle=circle)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=str(exc),
        ) from exc
    if not found:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )
    return OkResponse()


@router.post("/peers/{name}/touch", response_model=OkResponse)
async def touch_peer_last_seen(
    name: str,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Refresh a peer's last_seen without changing status.

    Called by MCP tool entry so outbound MCP traffic counts as a liveness
    signal — covers the case where a peer's ws-hook has dropped but the
    agent is still alive and acting (otherwise `is_orchestrator_present`
    and other last_seen-keyed checks go stale).
    """
    peer_registry = get_peer_registry()
    try:
        found = await peer_registry.touch_last_seen(name, circle=circle)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail=str(exc),
        ) from exc
    if not found:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )
    return OkResponse()


class TranscriptTurn(BaseModel):
    role: str
    text: str
    timestamp: str
    session_id: str
    turn_id: str
    tool_calls: list[dict[str, str]] = Field(default_factory=list)


class TranscriptResponse(BaseModel):
    turns: list[TranscriptTurn]
    next_before: str | None = None
    history_status: str = "available"
    history_backend: str = "claude-code"
    history_message: str = ""
    history_source: str = "peer_path"
    repowire_session_id: str | None = None
    binding_status: str | None = None
    runtime_session_id: str | None = None


class SessionTimelineItem(BaseModel):
    id: str
    kind: str
    source: str
    timestamp: str
    session_id: str
    turn_id: str
    role: str
    text: str
    tool_calls: list[dict[str, str]] = Field(default_factory=list)
    peer_id: str | None = None
    peer: str | None = None
    event_ids: list[str] = Field(default_factory=list)


class SessionTimelineResponse(BaseModel):
    peer_id: str
    peer_name: str
    session_id: str | None = None
    history_status: str = "available"
    history_backend: str = "claude-code"
    history_message: str = ""
    history_source: str = "peer_path"
    repowire_session_id: str | None = None
    binding_status: str | None = None
    runtime_session_id: str | None = None
    items: list[SessionTimelineItem]


def _timeline_item_response(item: TimelineItem) -> SessionTimelineItem:
    return SessionTimelineItem(
        id=item.id,
        kind=item.kind,
        source=item.source,
        timestamp=item.timestamp,
        session_id=item.session_id,
        turn_id=item.turn_id,
        role=item.role,
        text=item.text,
        tool_calls=item.tool_calls,
        peer_id=item.peer_id,
        peer=item.peer,
        event_ids=item.event_ids,
    )


def _resolve_history_binding(peer: Peer, session_id: str | None) -> SessionBinding | None:
    """Return an unambiguous binding for compatibility history routes."""
    binding_store = getattr(get_app_state(), "session_binding_store", None)
    if binding_store is None:
        return None

    backend = peer.backend.value
    project_path = peer.path or None
    if session_id:
        binding = binding_store.get_by_runtime_session(
            session_id,
            backend=backend,
            project_path=project_path,
        )
        if binding is not None:
            return binding

        # Some early observations may lack a project path; keep the runtime
        # session boundary useful without treating display name as durable.
        return binding_store.get_by_runtime_session(session_id, backend=backend)

    bindings = binding_store.list_by_peer(peer.peer_id)
    usable = [
        binding for binding in bindings
        if binding.backend == backend and binding.project_path == (peer.path or "")
    ]
    if len(usable) == 1:
        return usable[0]

    hook_session_id = peer.metadata.get("hook_session_id")
    if isinstance(hook_session_id, str) and hook_session_id:
        for binding in usable:
            if binding.runtime_session_id == hook_session_id:
                return binding
    return None


def _load_history_for_peer(
    peer: Peer,
    session_id: str | None,
) -> tuple[HistoryLoadResult, SessionBinding | None]:
    binding = _resolve_history_binding(peer, session_id)
    if binding is not None:
        history = load_bound_history(
            peer_path=binding.project_path or peer.path,
            backend=binding.backend,
            runtime_session_id=binding.runtime_session_id or session_id,
            runtime_source_uri=binding.runtime_source_uri,
            metadata={**peer.metadata, **binding.metadata},
        )
        return history, binding
    return load_peer_history(peer.path, peer.backend, peer.metadata), None


@router.get("/peers/{name}/timeline", response_model=SessionTimelineResponse)
async def get_peer_timeline(
    name: str,
    limit: int = Query(100, ge=1, le=500, description="Max timeline items to return"),
    session_id: str | None = Query(
        None,
        description="Optional hook/runtime transcript session id used for timeline scoping.",
    ),
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> SessionTimelineResponse:
    """Merged session timeline for dashboard/control callers.

    v0.13-compatible slice: persisted backend history turns and buffered
    realtime ``chat_turn``/``chat_turn_delta`` events are normalized to one
    session/turn identity model. Existing event endpoints remain unchanged.
    """
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer(name, circle=circle)
    if peer is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

    history, binding = await asyncio.to_thread(_load_history_for_peer, peer, session_id)
    items = build_session_timeline(
        history_turns=history.turns,
        events=peer_registry.get_events(),
        peer_id=peer.peer_id,
        peer_names={peer.display_name, peer.peer_id, name},
        session_id=session_id,
    )
    if len(items) > limit:
        items = items[-limit:]
    return SessionTimelineResponse(
        peer_id=peer.peer_id,
        peer_name=peer.display_name,
        session_id=session_id,
        history_status=history.status,
        history_backend=history.backend,
        history_message=history.message,
        history_source="session_binding" if binding is not None else "peer_path",
        repowire_session_id=binding.repowire_session_id if binding else None,
        binding_status=binding.status if binding else None,
        runtime_session_id=binding.runtime_session_id if binding else session_id,
        items=[_timeline_item_response(item) for item in items],
    )


@router.get("/peers/{name}/transcript", response_model=TranscriptResponse)
async def get_peer_transcript(
    name: str,
    limit: int = Query(50, ge=1, le=500, description="Max turns to return"),
    before: str | None = Query(
        None,
        description=(
            "Opaque base64 cursor returned as `next_before` from a prior "
            "page; return turns strictly older than this position. The "
            "cursor encodes (timestamp, session_id, line_offset) for "
            "stable pagination across same-timestamp boundaries."
        ),
    ),
    session_id: str | None = Query(
        None,
        description=(
            "Optional hook/runtime transcript session id used for dashboard "
            "scoping. When set, "
            "only turns from that session are returned."
        ),
    ),
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> TranscriptResponse:
    """Paginated newest-first transcript for a peer's working directory.

    Reads supported backend-local history and returns explicit unsupported or
    unavailable status when no compatible history source exists.
    """
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer(name, circle=circle)
    if peer is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

    history, binding = await asyncio.to_thread(_load_history_for_peer, peer, session_id)
    turns = history.turns
    if session_id:
        turns = [turn for turn in turns if turn.session_id == session_id]
    page, next_before = page_turns(turns, limit, before)
    return TranscriptResponse(
        turns=[
            TranscriptTurn(
                role=t.role,
                text=t.text,
                timestamp=t.timestamp,
                session_id=t.session_id,
                turn_id=t.turn_id,
                tool_calls=t.tool_calls,
            )
            for t in page
        ],
        next_before=next_before,
        history_status=history.status,
        history_backend=history.backend,
        history_message=history.message,
        history_source="session_binding" if binding is not None else "peer_path",
        repowire_session_id=binding.repowire_session_id if binding else None,
        binding_status=binding.status if binding else None,
        runtime_session_id=binding.runtime_session_id if binding else session_id,
    )


class SetCircleRequest(BaseModel):
    """Request to set peer's circle."""

    peer_name: str = Field(..., min_length=1, description="Peer name")
    circle: str = Field(..., min_length=1, description="Circle to join")


@router.post("/peers/circle", response_model=OkResponse)
async def set_peer_circle_endpoint(
    request: SetCircleRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Set a peer's circle for cross-circle communication."""
    peer_registry = get_peer_registry()
    await peer_registry.set_peer_circle(request.peer_name, request.circle)
    return OkResponse()


class OrchestratorStatusResponse(BaseModel):
    """Liveness status of a circle's orchestrator."""

    circle: str
    present: bool
    peer_id: str | None = None
    peer_name: str | None = None
    last_seen: str | None = None
    stale_after_seconds: int


@router.get("/circles/{name}/orchestrator", response_model=OrchestratorStatusResponse)
async def get_circle_orchestrator(
    name: str,
    _: str | None = Depends(require_auth),
) -> OrchestratorStatusResponse:
    """Return the orchestrator presence status for a circle.

    `present=True` iff there's a peer with role=orchestrator, status online/busy,
    and last_seen within 2 * heartbeat_interval (one missed beat tolerated).
    """
    peer_registry = get_peer_registry()
    tolerance = peer_registry.heartbeat_tolerance()
    orch = await peer_registry.get_orchestrator(name)
    if orch is None:
        return OrchestratorStatusResponse(
            circle=name,
            present=False,
            stale_after_seconds=tolerance,
        )
    return OrchestratorStatusResponse(
        circle=name,
        present=True,
        peer_id=orch.peer_id,
        peer_name=orch.display_name,
        last_seen=orch.last_seen.isoformat() if orch.last_seen else None,
        stale_after_seconds=tolerance,
    )


# ---------------------------------------------------------------------------
# Per-peer MCP config (#183) — same-host only in v1
# ---------------------------------------------------------------------------


class McpServerResponse(BaseModel):
    name: str
    scope: str = "user"
    type: str = "stdio"
    command: str | None = None
    args: list[str] = Field(default_factory=list)
    url: str | None = None
    env_keys: list[str] = Field(default_factory=list)


class McpConfigScopeResponse(BaseModel):
    backend: str
    owner: str
    effective_scope: str
    label: str
    description: str
    supported_scopes: list[str] = Field(default_factory=list)
    default_scope: str = "user"
    is_global: bool = False
    peer_id: str
    peer_name: str
    project_path: str | None = None
    peer_machine: str | None = None
    self_machine: str
    same_host: bool


class McpListResponse(BaseModel):
    servers: list[McpServerResponse]
    config_scope: McpConfigScopeResponse | None = None


class McpMutationResponse(OkResponse):
    config_scope: McpConfigScopeResponse | None = None


class McpAddRequest(BaseModel):
    name: str = Field(..., min_length=1, description="Server name (unique per peer)")
    type: str = Field(default="stdio", description="stdio | http | sse")
    command: str | None = Field(default=None, description="Command for stdio type")
    args: list[str] = Field(default_factory=list)
    url: str | None = Field(default=None, description="URL for http/sse type")
    env: dict[str, str] = Field(default_factory=dict)


async def _resolve_peer_or_404(name: str, circle: str | None) -> Peer:
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer(name, circle=circle)
    if not peer:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )
    return peer


def _mcp_scope_metadata(peer: Peer) -> McpConfigScopeResponse:
    self_machine = socket.gethostname()
    backend = peer.backend
    if backend == AgentType.CLAUDE_CODE:
        scope = {
            "backend": backend.value,
            "owner": "peer/project",
            "effective_scope": "peer_project",
            "label": "Claude Code peer/project config",
            "description": (
                "Claude Code MCP edits can target user/global config or the peer's "
                "project/worktree via the selected add scope."
            ),
            "supported_scopes": ["user", "project"],
            "default_scope": "user",
            "is_global": False,
        }
    elif backend == AgentType.CODEX:
        scope = {
            "backend": backend.value,
            "owner": "backend",
            "effective_scope": "backend_global",
            "label": "Codex global backend config",
            "description": (
                "Codex MCP edits target the user-level Codex config shared by "
                "Codex sessions on this host."
            ),
            "supported_scopes": ["user"],
            "default_scope": "user",
            "is_global": True,
        }
    elif backend == AgentType.GEMINI:
        scope = {
            "backend": backend.value,
            "owner": "backend",
            "effective_scope": "backend_global",
            "label": "Gemini global backend config",
            "description": (
                "Gemini MCP edits target the user-level Gemini settings shared by "
                "Gemini sessions on this host."
            ),
            "supported_scopes": ["user"],
            "default_scope": "user",
            "is_global": True,
        }
    else:
        raise peer_mcp.NotSupportedError(f"MCP config not supported for backend {backend.value}")
    return McpConfigScopeResponse(
        **scope,
        peer_id=peer.peer_id,
        peer_name=peer.display_name,
        project_path=peer.path,
        peer_machine=peer.machine,
        self_machine=self_machine,
        same_host=not peer.machine or peer.machine == self_machine,
    )


def _optional_mcp_scope_metadata(peer: Peer) -> dict[str, Any] | None:
    try:
        return _mcp_scope_metadata(peer).model_dump()
    except peer_mcp.PeerMcpError:
        return None


def _enforce_local(peer: Peer) -> None:
    """Reject cross-host operations until ACP transport is wired (issue #183)."""
    self_machine = socket.gethostname()
    if peer.machine and peer.machine != self_machine:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "cross_host",
                "hint": (
                    "Per-peer MCP config is same-host only in v1; "
                    "ACP transport required for remote peers."
                ),
                "peer_machine": peer.machine,
                "self_machine": self_machine,
                "config_scope": _optional_mcp_scope_metadata(peer),
            },
        )


def _map_peer_mcp_error(e: peer_mcp.PeerMcpError) -> HTTPException:
    if isinstance(e, peer_mcp.ValidationError):
        return HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail=str(e))
    if isinstance(e, peer_mcp.NotSupportedError):
        return HTTPException(status_code=status.HTTP_501_NOT_IMPLEMENTED, detail=str(e))
    if isinstance(e, peer_mcp.DuplicateServerError):
        return HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(e))
    if isinstance(e, peer_mcp.ServerNotFoundError):
        return HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=str(e))
    if isinstance(e, peer_mcp.BackendTimeoutError):
        return HTTPException(status_code=status.HTTP_504_GATEWAY_TIMEOUT, detail=str(e))
    return HTTPException(status_code=status.HTTP_502_BAD_GATEWAY, detail=str(e))


@router.get("/peers/{name}/mcp", response_model=McpListResponse)
async def list_peer_mcp(
    name: str,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> McpListResponse:
    """List MCP servers configured for the peer's backend.

    Same-host only — returns 409 cross_host for remote peers.
    """
    peer = await _resolve_peer_or_404(name, circle)
    _enforce_local(peer)
    try:
        config_scope = _mcp_scope_metadata(peer)
        entries = await asyncio.to_thread(peer_mcp.list_servers, peer)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    return McpListResponse(
        servers=[McpServerResponse(**e.to_dict()) for e in entries],
        config_scope=config_scope,
    )


@router.post("/peers/{name}/mcp", response_model=McpMutationResponse)
async def add_peer_mcp(
    name: str,
    request: McpAddRequest,
    circle: str | None = Query(None),
    scope: str = Query("user", description="user | project (claude-code only)"),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Add an MCP server to the peer's backend config."""
    peer = await _resolve_peer_or_404(name, circle)
    _enforce_local(peer)
    try:
        config_scope = _mcp_scope_metadata(peer)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    if scope not in config_scope.supported_scopes:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "unsupported_scope",
                "hint": (
                    f"Backend {peer.backend.value} supports MCP scopes: "
                    f"{', '.join(config_scope.supported_scopes)}"
                ),
                "requested_scope": scope,
                "supported_scopes": config_scope.supported_scopes,
                "config_scope": config_scope.model_dump(),
            },
        )
    spec = peer_mcp.McpServerSpec(
        name=request.name,
        type=request.type,
        command=request.command,
        args=list(request.args),
        url=request.url,
        env=dict(request.env),
        scope=scope,
    )
    try:
        await asyncio.to_thread(peer_mcp.add_server, peer, spec)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    return McpMutationResponse(config_scope=config_scope)


@router.delete("/peers/{name}/mcp/{server_name}", response_model=McpMutationResponse)
async def remove_peer_mcp(
    name: str,
    server_name: str,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Remove an MCP server from the peer's backend config."""
    peer = await _resolve_peer_or_404(name, circle)
    _enforce_local(peer)
    try:
        config_scope = _mcp_scope_metadata(peer)
        await asyncio.to_thread(peer_mcp.remove_server, peer, server_name)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    return McpMutationResponse(config_scope=config_scope)


# Legacy endpoints for backward compatibility


@router.post("/peer/register", response_model=RegisterResponse)
async def register_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> RegisterResponse:
    """Register a new peer in the mesh (legacy endpoint)."""
    peer_id, display_name = await _register_peer_impl(request)
    return RegisterResponse(peer_id=peer_id, display_name=display_name)


@router.post("/peer/unregister", response_model=OkResponse)
async def unregister_peer(
    request: UnregisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer from the mesh (legacy endpoint)."""
    await _unregister_peer_impl(request.name)
    return OkResponse()
