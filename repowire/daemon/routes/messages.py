"""Message handling endpoints.

TODO(relay): query/notify/broadcast are unauthenticated in local mode.
When going hosted, enforce auth on all message endpoints and add input
size limits to prevent abuse. Grep for TODO(relay) to find all sites.
"""

from __future__ import annotations

import asyncio
import json
from typing import Literal

from fastapi import APIRouter, Depends, HTTPException, status
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

from repowire.config.models import DEFAULT_QUERY_TIMEOUT
from repowire.daemon.auth import require_auth, require_localhost
from repowire.daemon.deps import get_app_state, get_peer_manager
from repowire.protocol.peers import PeerStatus

router = APIRouter(tags=["messages"])


class QueryRequest(BaseModel):
    """Request to query a peer."""

    from_peer: str | None = Field(None, description="Name of the sending peer (optional for CLI)")
    to_peer: str = Field(..., description="Name of the target peer")
    text: str = Field(..., description="Query text")
    timeout: float = Field(default=DEFAULT_QUERY_TIMEOUT, description="Timeout in seconds")
    bypass_circle: bool = Field(default=False, description="Bypass circle restrictions (CLI mode)")
    circle: str | None = Field(None, description="Circle to scope target peer lookup")


class QueryResponse(BaseModel):
    """Response from a query."""

    text: str | None = None
    error: str | None = None
    status: str | None = None  # PeerStatus.BUSY.value or PeerStatus.OFFLINE.value if rejected


class NotifyRequest(BaseModel):
    """Request to send a notification."""

    from_peer: str = Field(..., description="Name of the sending peer")
    to_peer: str = Field(..., description="Name of the target peer")
    text: str = Field(..., description="Notification text")
    bypass_circle: bool = Field(default=False, description="Bypass circle restrictions (CLI mode)")
    circle: str | None = Field(None, description="Circle to scope target peer lookup")


class BroadcastRequest(BaseModel):
    """Request to broadcast a message."""

    from_peer: str = Field(..., description="Name of the sending peer")
    text: str = Field(..., description="Broadcast text")
    exclude: list[str] = Field(default_factory=list, description="Peers to exclude")
    bypass_circle: bool = Field(default=False, description="Bypass circle restrictions (CLI mode)")


class BroadcastResponse(BaseModel):
    """Response from a broadcast."""

    ok: bool = True
    sent_to: list[str]


class SessionUpdateRequest(BaseModel):
    """Request to update session status."""

    peer_name: str | None = Field(None, description="Peer name")
    pane_id: str | None = Field(None, description="Tmux pane ID (alternative to peer_name)")
    status: str = Field(..., description="New status (online, busy, offline)")
    metadata: dict | None = Field(None, description="Optional metadata")


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True


@router.post("/query", response_model=QueryResponse)
async def query_peer(
    request: QueryRequest,
    _: str | None = Depends(require_auth),
) -> QueryResponse:
    """Send a query to a peer and wait for response."""
    peer_manager = get_peer_manager()
    await peer_manager.lazy_repair()

    # Check peer state before attempting query
    peer = await peer_manager.get_peer(request.to_peer, circle=request.circle)
    if peer:
        if peer.status == PeerStatus.BUSY:
            return QueryResponse(
                error=f"Peer '{request.to_peer}' is busy",
                status=PeerStatus.BUSY.value,
            )
        if peer.status == PeerStatus.OFFLINE:
            return QueryResponse(
                error=f"Peer '{request.to_peer}' is offline",
                status=PeerStatus.OFFLINE.value,
            )

    # Use "cli" as default from_peer if not specified
    from_peer = request.from_peer or "cli"
    # Auto-bypass circles for CLI requests (when from_peer was not specified)
    bypass = request.bypass_circle or request.from_peer is None

    try:
        response_text = await peer_manager.query(
            from_peer=from_peer,
            to_peer=request.to_peer,
            text=request.text,
            timeout=request.timeout,
            bypass_circle=bypass,
            circle=request.circle,
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
    await peer_manager.lazy_repair()

    try:
        await peer_manager.notify(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            text=request.text,
            bypass_circle=request.bypass_circle,
            circle=request.circle,
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
    await peer_manager.lazy_repair()

    sent_to = await peer_manager.broadcast(
        from_peer=request.from_peer,
        text=request.text,
        exclude=request.exclude,
        bypass_circle=request.bypass_circle,
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

    # Resolve peer identifier
    if request.peer_name:
        identifier = request.peer_name
    elif request.pane_id:
        peer = await peer_manager.get_peer_by_pane(request.pane_id)
        if not peer:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail=f"No peer for pane: {request.pane_id}",
            )
        identifier = peer.peer_id
    else:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="Either peer_name or pane_id required",
        )

    await peer_manager.update_peer_status(identifier, peer_status)
    return OkResponse()


class ResponseDelivery(BaseModel):
    """Response delivered by stop hook."""

    pane_id: str = Field(..., description="Tmux pane ID of the responding peer")
    text: str = Field(..., description="Response text")


@router.post("/response", response_model=OkResponse)
async def deliver_response(
    request: ResponseDelivery,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Receive response from stop hook and resolve pending query."""
    peer_manager = get_peer_manager()
    peer = await peer_manager.get_peer_by_pane(request.pane_id)
    if not peer:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"No peer for pane: {request.pane_id}",
        )

    state = get_app_state()
    query_tracker = state.query_tracker
    resolved = query_tracker.resolve_oldest_query(peer.peer_id, request.text)
    if not resolved:
        # No pending query — not an error, stop hook fires on every turn
        pass
    return OkResponse()


class ChatTurnRequest(BaseModel):
    """Request to ingest a chat turn."""

    peer: str
    role: Literal["user", "assistant"]
    text: str


@router.post("/events/chat", response_model=OkResponse)
async def ingest_chat_turn(
    request: ChatTurnRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Ingest a chat turn from the stop hook for dashboard display."""
    peer_manager = get_peer_manager()
    peer_manager.add_event("chat_turn", request.model_dump())
    return OkResponse()


@router.get("/events")
async def get_events(
    _: str | None = Depends(require_auth),
) -> list[dict]:
    """Get the last 100 communication events."""
    peer_manager = get_peer_manager()
    return peer_manager.get_events()


@router.get("/events/stream")
async def stream_events(
    _: None = Depends(require_localhost),
) -> StreamingResponse:
    """Stream events via Server-Sent Events (SSE).

    Clients connect once and receive events as they occur.
    Restricted to localhost.
    """
    peer_manager = get_peer_manager()

    async def event_generator():
        last_event_id: str | None = None
        while True:
            events = peer_manager.get_events()

            # Find new events since last seen ID
            if not events:
                await asyncio.sleep(0.5)
                continue

            if last_event_id is None:
                # First poll: send all current events
                for event in events:
                    yield f"data: {json.dumps(event)}\n\n"
                last_event_id = events[-1]["id"]
            else:
                # Find index after last seen event
                new_events = []
                seen = False
                for event in events:
                    if seen:
                        new_events.append(event)
                    elif event["id"] == last_event_id:
                        seen = True

                if not seen:
                    # last_event_id was evicted from deque; send all
                    new_events = events

                for event in new_events:
                    yield f"data: {json.dumps(event)}\n\n"

                if new_events:
                    last_event_id = new_events[-1]["id"]

            await asyncio.sleep(0.5)

    return StreamingResponse(
        event_generator(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",  # Disable nginx buffering
        },
    )
