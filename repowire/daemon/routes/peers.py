"""Peer management endpoints."""

from __future__ import annotations

import socket
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_config, get_peer_manager
from repowire.protocol.peers import Peer, PeerStatus

router = APIRouter(tags=["peers"])


class PeerInfo(BaseModel):
    """Peer information for API responses."""

    name: str
    path: str | None = None
    machine: str | None = None
    tmux_session: str | None = None
    opencode_url: str | None = None
    circle: str = "global"
    status: str
    last_seen: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class PeersResponse(BaseModel):
    """Response containing list of peers."""

    peers: list[PeerInfo]


class RegisterPeerRequest(BaseModel):
    """Request to register a peer."""

    name: str = Field(..., description="Peer name")
    path: str | None = Field(None, description="Working directory path")
    machine: str | None = Field(None, description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session:window")
    opencode_url: str | None = Field(None, description="OpenCode server URL")
    circle: str | None = Field(None, description="Circle (logical subnet)")
    metadata: dict[str, Any] = Field(default_factory=dict)


class UnregisterPeerRequest(BaseModel):
    """Request to unregister a peer."""

    name: str = Field(..., description="Peer name to unregister")


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True


@router.get("/peers", response_model=PeersResponse)
async def list_peers(
    _: str | None = Depends(require_auth),
) -> PeersResponse:
    """Get list of all registered peers."""
    peer_manager = get_peer_manager()
    peers = await peer_manager.get_all_peers()

    return PeersResponse(
        peers=[
            PeerInfo(
                name=p.name,
                path=p.path,
                machine=p.machine,
                tmux_session=p.tmux_session,
                opencode_url=getattr(p, "opencode_url", None),
                circle=p.circle,
                status=p.status.value,
                last_seen=p.last_seen.isoformat() if p.last_seen else None,
                metadata=p.metadata,
            )
            for p in peers
        ]
    )


@router.get("/peers/{name}", response_model=PeerInfo)
async def get_peer(
    name: str,
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get information about a specific peer."""
    peer_manager = get_peer_manager()
    peers = await peer_manager.get_all_peers()

    for p in peers:
        if p.name == name:
            return PeerInfo(
                name=p.name,
                path=p.path,
                machine=p.machine,
                tmux_session=p.tmux_session,
                opencode_url=getattr(p, "opencode_url", None),
                circle=p.circle,
                status=p.status.value,
                last_seen=p.last_seen.isoformat() if p.last_seen else None,
                metadata=p.metadata,
            )

    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail=f"Peer not found: {name}",
    )


@router.post("/peers", response_model=OkResponse)
async def create_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Register a new peer (CLI-friendly endpoint)."""
    config = get_config()

    # Add to config (persisted)
    config.add_peer(
        name=request.name,
        path=request.path,
        tmux_session=request.tmux_session,
        opencode_url=request.opencode_url,
        circle=request.circle,
    )

    # Also register with peer manager for immediate use
    peer_manager = get_peer_manager()
    # Resolve circle using peer manager's logic
    peer_config = config.get_peer(request.name)
    circle = peer_manager.resolve_circle(peer_config) if peer_config else "global"

    peer = Peer(
        name=request.name,
        path=request.path or "",
        machine=request.machine or socket.gethostname(),
        tmux_session=request.tmux_session,
        circle=circle,
        status=PeerStatus.ONLINE,
        metadata=request.metadata,
    )
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

    # Remove from config
    removed_from_config = config.remove_peer(name)

    # Remove from peer manager
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
    """Set a peer's circle for cross-backend communication.

    Allows peers to join named circles to communicate with peers from
    different backends (e.g., claudemux peer joining OpenCode's circle).
    """
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

    peer = Peer(
        name=request.name,
        path=request.path or "",
        machine=request.machine or socket.gethostname(),
        tmux_session=request.tmux_session,
        status=PeerStatus.ONLINE,
        metadata=request.metadata,
    )

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
