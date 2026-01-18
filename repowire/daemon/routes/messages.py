"""Message handling endpoints."""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_backend, get_peer_manager
from repowire.protocol.peers import PeerStatus

router = APIRouter(tags=["messages"])


class QueryRequest(BaseModel):
    """Request to query a peer."""

    from_peer: str | None = Field(None, description="Name of the sending peer (optional for CLI)")
    to_peer: str = Field(..., description="Name of the target peer")
    text: str = Field(..., description="Query text")
    timeout: float = Field(default=120.0, description="Timeout in seconds")


class QueryResponse(BaseModel):
    """Response from a query."""

    text: str | None = None
    error: str | None = None


class NotifyRequest(BaseModel):
    """Request to send a notification."""

    from_peer: str = Field(..., description="Name of the sending peer")
    to_peer: str = Field(..., description="Name of the target peer")
    text: str = Field(..., description="Notification text")


class BroadcastRequest(BaseModel):
    """Request to broadcast a message."""

    from_peer: str = Field(..., description="Name of the sending peer")
    text: str = Field(..., description="Broadcast text")
    exclude: list[str] = Field(default_factory=list, description="Peers to exclude")


class BroadcastResponse(BaseModel):
    """Response from a broadcast."""

    ok: bool = True
    sent_to: list[str]


class SessionUpdateRequest(BaseModel):
    """Request to update session status."""

    peer_name: str = Field(..., description="Peer name")
    status: str = Field(..., description="New status (online, busy, offline)")
    metadata: dict | None = Field(None, description="Optional metadata")


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True


class HookResponseRequest(BaseModel):
    """Request from Stop hook with captured response."""

    correlation_id: str = Field(..., description="Correlation ID of the pending query")
    response: str = Field(..., description="Captured response text")


@router.post("/query", response_model=QueryResponse)
async def query_peer(
    request: QueryRequest,
    _: str | None = Depends(require_auth),
) -> QueryResponse:
    """Send a query to a peer and wait for response."""
    peer_manager = get_peer_manager()

    # Use "cli" as default from_peer if not specified
    from_peer = request.from_peer or "cli"

    try:
        response_text = await peer_manager.query(
            from_peer=from_peer,
            to_peer=request.to_peer,
            text=request.text,
            timeout=request.timeout,
        )
        return QueryResponse(text=response_text)
    except ValueError as e:
        return QueryResponse(error=str(e))
    except TimeoutError:
        return QueryResponse(error=f"Timeout waiting for {request.to_peer}")
    except Exception as e:
        return QueryResponse(error=f"Query failed: {e}")


@router.post("/notify", response_model=OkResponse)
async def notify_peer(
    request: NotifyRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Send a notification to a peer (fire-and-forget)."""
    peer_manager = get_peer_manager()

    try:
        await peer_manager.notify(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            text=request.text,
        )
        return OkResponse()
    except ValueError as e:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=str(e),
        )
    except Exception as e:
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail=f"Failed to send notification: {e}",
        )


@router.post("/broadcast", response_model=BroadcastResponse)
async def broadcast_message(
    request: BroadcastRequest,
    _: str | None = Depends(require_auth),
) -> BroadcastResponse:
    """Broadcast a message to all peers."""
    peer_manager = get_peer_manager()

    sent_to = await peer_manager.broadcast(
        from_peer=request.from_peer,
        text=request.text,
        exclude=request.exclude,
    )

    return BroadcastResponse(sent_to=sent_to)


@router.post("/session/update", response_model=OkResponse)
async def update_session(
    request: SessionUpdateRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Update session status for a peer."""
    peer_manager = get_peer_manager()

    try:
        peer_status = PeerStatus(request.status)
    except ValueError:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail=f"Invalid status: {request.status}. Must be one of: online, busy, offline",
        )

    updated = await peer_manager.update_peer_status(request.peer_name, peer_status)
    if not updated:
        # Peer might not be registered yet - that's okay for session updates
        pass

    return OkResponse()


@router.post("/hook/response", response_model=OkResponse)
async def hook_response(request: HookResponseRequest) -> OkResponse:
    """Receive response from Stop hook (no auth - called by local hooks)."""
    backend = get_backend()

    # Only claudemux backend supports resolve_query
    if hasattr(backend, "resolve_query"):
        backend.resolve_query(request.correlation_id, request.response)

    return OkResponse()
