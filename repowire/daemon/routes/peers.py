"""Peer management endpoints."""

from __future__ import annotations

import socket
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Query, status
from pydantic import BaseModel, Field, field_validator

from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_peer_registry
from repowire.daemon.routes._shared import OkResponse, is_valid_identifier
from repowire.protocol.peers import Peer, PeerRole

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
        last_seen=p.last_seen.isoformat() if p.last_seen else None,
        metadata=p.metadata,
        description=p.description,
    )


class PeersResponse(BaseModel):
    """Response containing list of peers."""

    peers: list[PeerInfo]


class RegisterPeerRequest(BaseModel):
    """Request to register a peer."""

    name: str = Field(..., min_length=1, pattern=r"^[a-zA-Z0-9._-]+$", description="Peer name")
    path: str | None = Field(None, description="Working directory path")
    machine: str | None = Field(None, description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session:window")
    backend: AgentType = Field(default=AgentType.CLAUDE_CODE, description="Agent type")
    circle: str | None = Field(None, description="Circle (logical subnet)")
    role: PeerRole = Field(default=PeerRole.AGENT, description="Peer role")
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



@router.get("/peers", response_model=PeersResponse)
async def list_peers(
    _: str | None = Depends(require_auth),
) -> PeersResponse:
    """Get list of all registered peers."""
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()
    peers = await peer_registry.get_all_peers()
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


@router.get("/peers/{identifier}", response_model=PeerInfo)
async def get_peer(
    identifier: str,
    circle: str | None = Query(None),
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get information about a specific peer by peer_id or display_name."""
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer(identifier, circle=circle)
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
    peer_id, display_name = await peer_registry.allocate_and_register(
        circle=circle,
        backend=request.backend,
        path=request.path or "",
        tmux_session=request.tmux_session,
        metadata=request.metadata,
        machine=request.machine or socket.gethostname(),
        role=request.role,
    )
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
    found = await peer_registry.update_description(name, request.description, circle=circle)
    if not found:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )
    return OkResponse()


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
