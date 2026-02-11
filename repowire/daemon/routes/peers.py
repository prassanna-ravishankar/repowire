"""Peer management endpoints."""

from __future__ import annotations

import socket
from typing import Any, cast

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import BackendType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_config, get_peer_manager
from repowire.protocol.peers import Peer, PeerStatus

router = APIRouter(tags=["peers"])


class PeerInfo(BaseModel):
    """Peer information for API responses."""

    peer_id: str
    pane_id: str  # Backward compat (deprecated, = peer_id)
    name: str  # Backward compat (= display_name)
    display_name: str
    path: str | None = None
    machine: str | None = None
    tmux_session: str | None = None
    backend: str = "claudemux"
    opencode_url: str | None = None
    circle: str = "global"
    status: str
    last_seen: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


def _peer_to_info(p: Peer) -> PeerInfo:
    """Convert a Peer model to a PeerInfo API response."""
    return PeerInfo(
        peer_id=p.peer_id,
        pane_id=p.peer_id,
        name=p.display_name,
        display_name=p.display_name,
        path=p.path,
        machine=p.machine,
        tmux_session=p.tmux_session,
        backend=p.backend,
        opencode_url=getattr(p, "opencode_url", None),
        circle=p.circle,
        status=p.status.value,
        last_seen=p.last_seen.isoformat() if p.last_seen else None,
        metadata=p.metadata,
    )


class PeersResponse(BaseModel):
    """Response containing list of peers."""

    peers: list[PeerInfo]


class RegisterPeerRequest(BaseModel):
    """Request to register a peer."""

    peer_id: str | None = Field(None, description="Unique peer ID (e.g., %42 or oc-xxxx)")
    pane_id: str | None = Field(None, description="Legacy: use peer_id instead")
    name: str = Field(..., description="Peer name (for backward compat)")
    display_name: str | None = Field(None, description="Human-readable name")
    path: str | None = Field(None, description="Working directory path")
    machine: str | None = Field(None, description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session:window")
    backend: str = Field(default="claudemux", description="Backend type")
    opencode_url: str | None = Field(None, description="OpenCode server URL")
    circle: str | None = Field(None, description="Circle (logical subnet)")
    metadata: dict[str, Any] = Field(default_factory=dict)


class UnregisterPeerRequest(BaseModel):
    """Request to unregister a peer."""

    name: str = Field(..., description="Peer name to unregister")


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True


def _resolve_peer_identity(request: RegisterPeerRequest) -> tuple[str, str]:
    """Resolve peer_id and display_name from a registration request.

    Returns:
        (peer_id, display_name) tuple
    """
    peer_id = request.peer_id or request.pane_id or f"legacy-{request.name}"
    display_name = request.display_name or request.name
    return peer_id, display_name


def _build_peer(
    request: RegisterPeerRequest, peer_id: str, display_name: str, circle: str = "global"
) -> Peer:
    """Build a Peer model from a registration request."""
    return Peer(
        peer_id=peer_id,
        display_name=display_name,
        path=request.path or "",
        machine=request.machine or socket.gethostname(),
        tmux_session=request.tmux_session,
        backend=cast(BackendType, request.backend),
        circle=circle,
        status=PeerStatus.ONLINE,
        metadata=request.metadata,
    )


@router.get("/peers", response_model=PeersResponse)
async def list_peers(
    _: str | None = Depends(require_auth),
) -> PeersResponse:
    """Get list of all registered peers."""
    peer_manager = get_peer_manager()
    peers = await peer_manager.get_all_peers()
    return PeersResponse(peers=[_peer_to_info(p) for p in peers])


@router.get("/peers/{identifier}", response_model=PeerInfo)
async def get_peer(
    identifier: str,
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get information about a specific peer by peer_id or display_name."""
    peer_manager = get_peer_manager()
    peers = await peer_manager.get_all_peers()

    for p in peers:
        if p.peer_id == identifier or p.display_name == identifier:
            return _peer_to_info(p)

    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail=f"Peer not found: {identifier}",
    )


@router.post("/peers", response_model=OkResponse)
async def create_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Register a new peer (CLI-friendly endpoint)."""
    config = get_config()
    peer_id, display_name = _resolve_peer_identity(request)

    # Persist to config
    config.add_peer(
        name=request.name,
        path=request.path,
        tmux_session=request.tmux_session,
        opencode_url=request.opencode_url,
        circle=request.circle,
        peer_id=peer_id,
        display_name=display_name,
    )

    # Register with peer manager for immediate use
    peer_manager = get_peer_manager()
    peer_config = config.get_peer(request.name)
    circle = peer_manager.resolve_circle(peer_config) if peer_config else "global"

    peer = _build_peer(request, peer_id, display_name, circle)
    await peer_manager.register_peer(peer)

    return OkResponse()


@router.delete("/peers/{name}", response_model=OkResponse)
async def delete_peer(
    name: str,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer by name (CLI-friendly endpoint)."""
    config = get_config()
    peer_manager = get_peer_manager()

    removed_from_config = config.remove_peer(name)
    removed_from_manager = await peer_manager.unregister_peer(name)

    if not removed_from_config and not removed_from_manager:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {name}",
        )

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
    peer_manager = get_peer_manager()
    cancelled = await peer_manager.mark_offline(name)
    return OfflineResponse(cancelled_queries=cancelled)


class SetCircleRequest(BaseModel):
    """Request to set peer's circle."""

    peer_name: str = Field(..., min_length=1, description="Peer name")
    circle: str = Field(..., min_length=1, description="Circle to join")


@router.post("/peers/circle", response_model=OkResponse)
async def set_peer_circle_endpoint(
    request: SetCircleRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Set a peer's circle for cross-backend communication."""
    peer_manager = get_peer_manager()
    await peer_manager.set_peer_circle(request.peer_name, request.circle)
    return OkResponse()


# Legacy endpoints for backward compatibility


@router.post("/peer/register", response_model=OkResponse)
async def register_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Register a new peer in the mesh (legacy endpoint)."""
    peer_manager = get_peer_manager()
    peer_id, display_name = _resolve_peer_identity(request)
    peer = _build_peer(request, peer_id, display_name)
    await peer_manager.register_peer(peer)
    return OkResponse()


@router.post("/peer/unregister", response_model=OkResponse)
async def unregister_peer(
    request: UnregisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer from the mesh (legacy endpoint)."""
    peer_manager = get_peer_manager()

    removed = await peer_manager.unregister_peer(request.name)
    if not removed:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {request.name}",
        )

    return OkResponse()
