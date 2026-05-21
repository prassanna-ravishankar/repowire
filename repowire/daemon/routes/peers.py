"""Peer management endpoints."""

from __future__ import annotations

import asyncio
import os
import socket
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Query, status
from pydantic import BaseModel, Field, field_validator

from repowire import peer_mcp
from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_peer_registry
from repowire.daemon.peer_registry import (
    CircleSource,
    PaneHijackRejectedError,
    RoleClaimConflictError,
)
from repowire.daemon.routes._shared import OkResponse, is_valid_identifier
from repowire.protocol.peers import Peer, PeerRole, PeerStatus, TurnState
from repowire.session.history import load_peer_turns, page_turns

router = APIRouter(tags=["peers"])


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

    v1 reads Claude Code JSONLs under ~/.claude/projects/<encoded-cwd>/.
    Codex peers return an empty list (follow-up: rollout header scan).
    Returns 200 with empty turns when no transcript files exist.
    """
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer(name, circle=circle)
    if peer is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

    turns = await asyncio.to_thread(load_peer_turns, peer.path, peer.backend)
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


class McpListResponse(BaseModel):
    servers: list[McpServerResponse]


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
            },
        )


def _map_peer_mcp_error(e: peer_mcp.PeerMcpError) -> HTTPException:
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
        entries = await asyncio.to_thread(peer_mcp.list_servers, peer)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    return McpListResponse(servers=[McpServerResponse(**e.to_dict()) for e in entries])


@router.post("/peers/{name}/mcp", response_model=OkResponse)
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
    return OkResponse()


@router.delete("/peers/{name}/mcp/{server_name}", response_model=OkResponse)
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
        await asyncio.to_thread(peer_mcp.remove_server, peer, server_name)
    except peer_mcp.PeerMcpError as e:
        raise _map_peer_mcp_error(e) from e
    return OkResponse()


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
