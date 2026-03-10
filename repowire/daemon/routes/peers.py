"""Peer management endpoints."""

from __future__ import annotations

import socket
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Query, status
from pydantic import BaseModel, Field, field_validator

from repowire.config.models import AgentType
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_manager
from repowire.protocol.peers import Peer, PeerStatus

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
    status: str
    last_seen: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


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
        status=p.status.value,
        last_seen=p.last_seen.isoformat() if p.last_seen else None,
        metadata=p.metadata,
    )


class PeersResponse(BaseModel):
    """Response containing list of peers."""

    peers: list[PeerInfo]


class RegisterPeerRequest(BaseModel):
    """Request to register a peer."""

    name: str = Field(..., min_length=1, pattern=r"^[a-zA-Z0-9._-]+$", description="Peer name")
    display_name: str | None = Field(None, description="Human-readable name")
    path: str | None = Field(None, description="Working directory path")
    machine: str | None = Field(None, description="Machine hostname")
    tmux_session: str | None = Field(None, description="Tmux session:window")
    backend: AgentType = Field(default=AgentType.CLAUDE_CODE, description="Agent type")
    circle: str | None = Field(None, description="Circle (logical subnet)")
    metadata: dict[str, Any] = Field(default_factory=dict)

    @field_validator("circle")
    @classmethod
    def validate_circle(cls, v: str | None) -> str | None:
        if v is not None:
            import re

            if not re.match(r"^[a-zA-Z0-9._-]+$", v) or len(v) > 64:
                raise ValueError("Circle must match ^[a-zA-Z0-9._-]+$ and be <= 64 chars")
        return v


class UnregisterPeerRequest(BaseModel):
    """Request to unregister a peer."""

    name: str = Field(..., description="Peer name to unregister")


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True


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
        backend=request.backend,
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
    await peer_manager.lazy_repair()
    peers = await peer_manager.get_all_peers()
    return PeersResponse(peers=[_peer_to_info(p) for p in peers])


@router.get("/peers/by-pane/{pane_id}", response_model=PeerInfo)
async def get_peer_by_pane(
    pane_id: str,
    _: str | None = Depends(require_auth),
) -> PeerInfo:
    """Get peer by tmux pane ID."""
    peer_manager = get_peer_manager()
    peer = await peer_manager.get_peer_by_pane(pane_id)
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
    peer_manager = get_peer_manager()
    peer = await peer_manager.get_peer(identifier, circle=circle)
    if peer:
        return _peer_to_info(peer)

    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail=f"Peer not found: {identifier}",
    )


async def _register_peer_impl(request: RegisterPeerRequest) -> None:
    """Shared implementation for peer registration endpoints."""
    display_name = request.display_name or request.name
    circle = request.circle or "global"

    state = get_app_state()
    session_id = state.session_mapper.register_session(
        display_name=display_name,
        path=request.path or "",
        circle=circle,
        backend=request.backend,
    )

    peer = _build_peer(request, session_id, display_name, circle)

    peer_manager = get_peer_manager()
    await peer_manager.register_peer(peer)


@router.post("/peers", response_model=OkResponse)
async def create_peer(
    request: RegisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Register a new peer (CLI-friendly endpoint)."""
    await _register_peer_impl(request)
    return OkResponse()


@router.delete("/peers/{name}", response_model=OkResponse)
async def delete_peer(
    name: str,
    circle: str | None = Query(None, description="Circle to scope deletion to avoid ambiguity"),
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer by name (CLI-friendly endpoint)."""
    peer_manager = get_peer_manager()
    removed = await peer_manager.unregister_peer(name, circle=circle)

    # Clean up SessionMapper to prevent ghost peers
    state = get_app_state()
    session_mapper = state.session_mapper
    for sid, mapping in list(session_mapper.get_all_mappings().items()):
        if mapping.display_name == name:
            if circle and mapping.circle != circle:
                continue
            session_mapper.unregister_session(sid)
            removed = True

    if not removed:
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
    """Set a peer's circle for cross-circle communication."""
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
    await _register_peer_impl(request)
    return OkResponse()


@router.post("/peer/unregister", response_model=OkResponse)
async def unregister_peer(
    request: UnregisterPeerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Unregister a peer from the mesh (legacy endpoint)."""
    peer_manager = get_peer_manager()

    removed = await peer_manager.unregister_peer(request.name)

    # Clean up SessionMapper to prevent ghost peers
    state = get_app_state()
    session_mapper = state.session_mapper
    for sid, mapping in list(session_mapper.get_all_mappings().items()):
        if mapping.display_name == request.name:
            session_mapper.unregister_session(sid)
            removed = True

    if not removed:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Peer not found: {request.name}",
        )

    return OkResponse()
