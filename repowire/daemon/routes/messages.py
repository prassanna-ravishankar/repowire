"""Message handling endpoints."""

from __future__ import annotations

import asyncio
import json
import uuid
from typing import Literal

from fastapi import APIRouter, Depends, HTTPException, Query, status
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

from repowire.acp import deliver_ask_via_acp, maybe_decide_acp_route
from repowire.config.models import DEFAULT_QUERY_TIMEOUT
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.routes._shared import OkResponse
from repowire.protocol.peers import PeerStatus, TurnState

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


class NotifyResponse(BaseModel):
    """Response from /notify.

    ``status`` reflects the recipient's state at send-time: ``sent`` means
    ONLINE (immediate paste), ``queued`` means BUSY (ws-hook holds the paste
    until the current turn ends). Wire format otherwise unchanged.

    Notify is fire-and-forget: daemon-side success means the frame was handed
    to the recipient WebSocket, not that the agent received or processed it.
    Use ask/ack when confirmed delivery matters.
    """

    ok: bool = True
    status: Literal["sent", "queued"] = "sent"


class BroadcastRequest(BaseModel):
    """Request to broadcast a message."""

    from_peer: str = Field(..., description="Name of the sending peer")
    text: str = Field(..., description="Broadcast text")
    exclude: list[str] = Field(default_factory=list, description="Peers to exclude")
    bypass_circle: bool = Field(default=False, description="Bypass circle restrictions (CLI mode)")


class BroadcastResponse(BaseModel):
    """Response from a broadcast. Best-effort per-recipient."""

    ok: bool = True
    sent_to: list[str]
    failed: list[dict[str, str]] = Field(default_factory=list)


class SessionUpdateRequest(BaseModel):
    """Request to update session status."""

    peer_name: str | None = Field(None, description="Peer name")
    pane_id: str | None = Field(None, description="Tmux pane ID (alternative to peer_name)")
    status: str | None = Field(
        None,
        description=(
            "New status (online, busy, offline). Optional when only "
            "turn_state is being updated."
        ),
    )
    turn_state: TurnState | None = Field(
        None,
        description=(
            "New turn_state (idle, working, awaiting_input, "
            "pending_first_turn). Orthogonal to status."
        ),
    )
    metadata: dict | None = Field(None, description="Optional metadata")


@router.post("/query", response_model=QueryResponse)
async def query_peer(
    request: QueryRequest,
    _: str | None = Depends(require_auth),
) -> QueryResponse:
    """Send a query to a peer and wait for response."""
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()

    # Check peer state before attempting query
    peer = await peer_registry.get_peer(request.to_peer, circle=request.circle)
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
        response_text = await peer_registry.query(
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


@router.post("/notify", response_model=NotifyResponse)
async def notify_peer(
    request: NotifyRequest,
    _: str | None = Depends(require_auth),
) -> NotifyResponse:
    """Send a notification to a peer (fire-and-forget).

    Routing is transport-agnostic:

      * ACP-marked target peer + ``experiments.acp_broker_client`` flag on →
        delivered as a fire-and-forget ACP prompt against the peer's
        subprocess. There is no notify primitive in ACP; we map notify onto
        prompt and discard the assembled reply so the call stays
        fire-and-forget. Returns ``status=sent`` once the prompt is dispatched.
      * All other peers → direct WS send. 503 if the recipient has no live
        connection so the caller knows to retry. Returns ``status=sent`` when
        the recipient was ONLINE at send-time, ``status=queued`` when BUSY
        (ws-hook holds the paste until the current turn ends).

    The ACP-first ordering is critical: an ACP peer never has a live WS
    connection, so the WS-presence check would 503 every brokered peer if it
    ran before the ACP decision (repowire#206).
    """
    from repowire.daemon.websocket_transport import TransportError

    peer_registry = get_peer_registry()
    state = get_app_state()
    await peer_registry.lazy_repair()

    # ACP routing decision happens before any WS dispatch so that ACP-marked
    # peers (which have no WS by design) don't 503 in the route layer.
    # check_access runs the same lookup + circle-boundary enforcement that
    # peer_registry.notify would apply on the WS path — we must not let the
    # ACP branch silently bypass circle gates.
    try:
        _from_obj, target = await peer_registry.check_access(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            bypass_circle=request.bypass_circle,
            circle=request.circle,
        )
    except ValueError as e:
        msg = str(e)
        if msg.startswith("Unknown peer"):
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND, detail=msg,
            ) from e
        if msg.startswith("Ambiguous peer name"):
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT, detail=msg,
            ) from e
        # Circle boundary violation → 403. This is the established "no
        # cross-circle traffic" semantic; previously the WS path bubbled
        # the same ValueError up to the catch-all 404 branch below, which
        # masked the real error. Keep this path consistent for ACP + WS.
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN, detail=msg,
        ) from e

    cfg = state.config
    acp_manager = getattr(state, "acp_manager", None)
    flag = bool(cfg.experiments.acp_broker_client) if cfg.experiments else False
    decision = maybe_decide_acp_route(target, flag_enabled=flag, manager=acp_manager)
    if decision is not None:
        assert acp_manager is not None  # narrowed by maybe_decide_acp_route
        assert decision.spec is not None

        async def _drop_result(_cid: str, _reply: str | None, _err: str | None) -> None:
            # Notify is fire-and-forget: any reply or error from the ACP turn
            # is logged inside deliver_ask_via_acp; nothing to deliver back.
            return None

        # Synthesize a correlation_id for log/cancel correlation only. No
        # ask_tracker registration: notify has no thread to close.
        cid = f"notif-{uuid.uuid4().hex[:8]}"
        await deliver_ask_via_acp(
            manager=acp_manager,
            spec=decision.spec,
            correlation_id=cid,
            text=request.text,
            on_complete=_drop_result,
        )
        return NotifyResponse(status="sent")

    try:
        delivery_status = await peer_registry.notify(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            text=request.text,
            bypass_circle=request.bypass_circle,
            circle=request.circle,
        )
        return NotifyResponse(status=delivery_status)
    except ValueError as e:
        # Ambiguous-display-name lookups → 409 (matches peer/description/touch/ask
        # paths). "Unknown peer" stays 404.
        msg = str(e)
        is_ambiguous = msg.startswith("Ambiguous peer name")
        raise HTTPException(
            status_code=(
                status.HTTP_409_CONFLICT if is_ambiguous else status.HTTP_404_NOT_FOUND
            ),
            detail=msg,
        )
    except TransportError as e:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail=f"Peer {request.to_peer} has no live connection: {e}",
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
    """Broadcast a message to all eligible peers. Best-effort per-recipient."""
    peer_registry = get_peer_registry()
    await peer_registry.lazy_repair()

    sent_to, failed = await peer_registry.broadcast(
        from_peer=request.from_peer,
        text=request.text,
        exclude=request.exclude,
        bypass_circle=request.bypass_circle,
    )

    return BroadcastResponse(sent_to=sent_to, failed=failed)


@router.post("/session/update", response_model=OkResponse)
async def update_session(
    request: SessionUpdateRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Update session status and/or turn_state for a peer."""
    peer_registry = get_peer_registry()

    if request.status is None and request.turn_state is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="At least one of status or turn_state is required",
        )

    peer_status: PeerStatus | None = None
    if request.status is not None:
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
        peer = await peer_registry.get_peer_by_pane(request.pane_id)
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

    if peer_status is not None:
        await peer_registry.update_peer_status(identifier, peer_status)
    if request.turn_state is not None:
        await peer_registry.update_peer_turn_state(identifier, request.turn_state)
    return OkResponse()


class ResponseDelivery(BaseModel):
    """Response delivered by stop hook."""

    pane_id: str = Field(..., description="Tmux pane ID of the responding peer")
    text: str = Field(..., description="Response text")
    correlation_id: str | None = Field(None, description="Correlation ID from pending query")


@router.post("/response", response_model=OkResponse)
async def deliver_response(
    request: ResponseDelivery,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Receive response from stop hook and resolve pending query."""
    peer_registry = get_peer_registry()
    peer = await peer_registry.get_peer_by_pane(request.pane_id)
    if not peer:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"No peer for pane: {request.pane_id}",
        )

    state = get_app_state()
    query_tracker = state.query_tracker
    if request.correlation_id:
        resolved = await query_tracker.resolve_query(request.correlation_id, request.text)
    else:
        resolved = await query_tracker.resolve_oldest_query(peer.peer_id, request.text)
    if not resolved:
        # No pending query — not an error, stop hook fires on every turn
        pass
    return OkResponse()


class ToolCallInfo(BaseModel):
    """Tool call summary."""

    name: str
    input: str = ""


class ChatTurnRequest(BaseModel):
    """Request to ingest a chat turn."""

    peer: str
    role: Literal["user", "assistant"]
    text: str
    tool_calls: list[ToolCallInfo] | None = None
    turn_id: str | None = Field(
        None,
        description=(
            "Assistant message uuid. When present, lets the dashboard reconcile "
            "streaming chat_turn_delta bubbles to this final turn deterministically "
            "and lets the daemon reject post-final late deltas."
        ),
    )
    peer_id: str | None = Field(None, description="Peer ID (if known)")
    pane_id: str | None = Field(None, description="Tmux pane ID (resolves peer_id server-side)")


# Bounded set of turn_ids that have already received their final chat_turn.
# Late chat_turn_delta posts for these ids are dropped — the dashboard would
# render them as orphan streaming bubbles otherwise.
#
# Capacity caps memory growth; FIFO eviction means very old turns may shadow
# new collisions but turn_ids are message-uuids so collisions don't occur in
# practice. Process-local (deltas/finals always pass through the same daemon).
_FINALIZED_TURN_IDS_CAPACITY: int = 4096
_finalized_turn_ids: dict[str, None] = {}


def _mark_turn_finalized(turn_id: str) -> None:
    if turn_id in _finalized_turn_ids:
        # Refresh recency so a finalized id stays in the set as long as deltas
        # might still trickle in.
        del _finalized_turn_ids[turn_id]
    _finalized_turn_ids[turn_id] = None
    while len(_finalized_turn_ids) > _FINALIZED_TURN_IDS_CAPACITY:
        _finalized_turn_ids.pop(next(iter(_finalized_turn_ids)))


def _is_turn_finalized(turn_id: str) -> bool:
    return turn_id in _finalized_turn_ids


@router.post("/events/chat", response_model=OkResponse)
async def ingest_chat_turn(
    request: ChatTurnRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Ingest a chat turn from the stop hook for dashboard display."""
    peer_registry = get_peer_registry()
    data = request.model_dump(exclude={"pane_id"})

    if not request.peer_id and request.pane_id:
        peer = await peer_registry.get_peer_by_pane(request.pane_id)
        if peer:
            data["peer_id"] = peer.peer_id
            data["peer"] = peer.display_name  # canonicalize to registered name

    if request.turn_id and request.role == "assistant":
        _mark_turn_finalized(request.turn_id)

    peer_registry.add_event("chat_turn", data)
    return OkResponse()


class ChatTurnDeltaRequest(BaseModel):
    """Request to ingest a partial chat-turn block while it streams.

    Block-level rather than token-level: each request carries one completed
    assistant text block or tool_use, as written to the transcript JSONL.
    The final ``chat_turn`` event remains authoritative for end-of-turn —
    deltas are additive, clients that ignore them keep working.
    """

    peer: str
    role: Literal["assistant"] = "assistant"
    turn_id: str = Field(..., description="Stable id for the assistant turn (deltas group by this)")
    chunk_index: int = Field(..., ge=0, description="Monotonic 0-based index within the turn")
    kind: Literal["text", "tool_use"] = Field(default="text", description="Block kind")
    text: str = Field("", description="Block content (full text block, or tool_use summary)")
    tool_call: ToolCallInfo | None = Field(None, description="Set when kind=tool_use")
    is_final: bool = Field(default=False, description="Hint: last delta in this turn")
    peer_id: str | None = Field(None, description="Peer ID (if known)")
    pane_id: str | None = Field(None, description="Tmux pane ID (resolves peer_id server-side)")


@router.post("/events/chat_delta", response_model=OkResponse)
async def ingest_chat_turn_delta(
    request: ChatTurnDeltaRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Ingest a streaming chat-turn delta from the per-pane transcript tailer.

    Drops deltas whose ``turn_id`` has already received its final ``chat_turn``
    — those would render as orphan streaming bubbles on the dashboard. Returns
    200 on drop so the streamer's best-effort post doesn't retry into a
    failure loop.
    """
    if _is_turn_finalized(request.turn_id):
        return OkResponse()

    peer_registry = get_peer_registry()
    data = request.model_dump(exclude={"pane_id"})

    if not request.peer_id and request.pane_id:
        peer = await peer_registry.get_peer_by_pane(request.pane_id)
        if peer:
            data["peer_id"] = peer.peer_id
            data["peer"] = peer.display_name

    peer_registry.add_event("chat_turn_delta", data)
    return OkResponse()


@router.get("/events")
async def get_events(
    since: str | None = Query(None, description="Return events after this event id"),
    _: str | None = Depends(require_auth),
) -> list[dict]:
    """Get communication events.

    Without ``since``: returns the full buffered window (last 500).
    With ``since``: returns events after the given id, or the full window if
    the id has been evicted from the buffer (gap-recovery fallback).
    """
    peer_registry = get_peer_registry()
    if since is None:
        return peer_registry.get_events()
    return peer_registry.events_since(since)


# Heartbeat interval for SSE keep-alives. Long enough that idle connections
# aren't chatty, short enough that proxies/clients notice a dead socket.
SSE_HEARTBEAT_SECS = 15.0


@router.get("/events/stream")
async def stream_events(
    _: str | None = Depends(require_auth),
) -> StreamingResponse:
    """Stream events via Server-Sent Events (SSE).

    Event-driven: blocks on an asyncio.Event set by ``add_event``, with a
    periodic comment-frame heartbeat so both ends detect dead connections.
    """
    peer_registry = get_peer_registry()

    async def event_generator():
        # Subscribe before the initial flush so events added concurrently
        # with the flush still wake us on the next loop iteration.
        wakeup = peer_registry.subscribe_events()
        last_event_id: str | None = None
        try:
            initial = peer_registry.get_events()
            for event in initial:
                yield f"data: {json.dumps(event)}\n\n"
            if initial:
                last_event_id = initial[-1]["id"]
            # Clear any signals raised during the initial flush; we've already
            # delivered everything currently buffered.
            wakeup.clear()

            while True:
                try:
                    await asyncio.wait_for(wakeup.wait(), timeout=SSE_HEARTBEAT_SECS)
                except asyncio.TimeoutError:
                    yield ": keepalive\n\n"
                    continue
                wakeup.clear()

                new_events = peer_registry.events_since(last_event_id)
                for event in new_events:
                    yield f"data: {json.dumps(event)}\n\n"
                if new_events:
                    last_event_id = new_events[-1]["id"]
        finally:
            peer_registry.unsubscribe_events(wakeup)

    return StreamingResponse(
        event_generator(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",  # Disable nginx buffering
        },
    )
