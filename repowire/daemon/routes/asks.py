"""Ask/ack lifecycle endpoints.

The non-blocking ask/ack model:

  POST /ask                       — register an ask, inject to recipient, return corr_id
  POST /ack                       — close an ask (bare or with reply content)
  POST /asks/{cid}/picked_up      — transport signals delivery (daemon snapshots turn_seq)
  GET  /asks/pending              — recipient's Stop hook polls for due reminders
  POST /asks/{cid}/mark_reminded  — recipient's Stop hook flips once-only reminder flag

The wire protocol carries these to peers as a first-class `type: ask`
message: `{type: ask, correlation_id, from_peer, text, reply_to}`. Each
transport (ws-hook, opencode plugin, channel server) dispatches `type=ask`
explicitly and POSTs `/asks/{cid}/picked_up` after presenting the message
to the agent.
"""

from __future__ import annotations

import logging

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.routes._shared import OkResponse
from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)
router = APIRouter(tags=["asks"])


class AskRequest(BaseModel):
    """Open a new ask thread."""

    from_peer: str = Field(..., description="Display name of the sender")
    to_peer: str = Field(..., description="Display name of the recipient")
    text: str = Field(..., description="Ask content")
    reply_to: str | None = Field(
        None,
        description="If set, closes the referenced ask AND opens this new one",
    )
    bypass_circle: bool = Field(default=False)
    circle: str | None = Field(None)


class AskResponse(BaseModel):
    """Result of opening an ask."""

    correlation_id: str
    status: str | None = None  # 'sent', 'queued' (recipient busy), or 'rejected'
    error: str | None = None


class AckRequest(BaseModel):
    """Close an ask. If `message` is set, IS the reply (delivered to asker)."""

    correlation_id: str
    message: str | None = None
    from_peer: str = Field(..., description="Display name of the acking peer")


class PickedUpRequest(BaseModel):
    """Transport reports that an ask was delivered to its recipient.

    Daemon owns turn_seq sequencing — it snapshots its own per-peer counter
    atomically when this endpoint is called. Clients only signal "delivered."
    """

    correlation_id: str
    pane_id: str | None = Field(None, description="Pane that picked up (for logging)")


class MarkRemindedRequest(BaseModel):
    correlation_id: str


class PendingAsk(BaseModel):
    correlation_id: str
    from_peer: str
    text: str
    created_at: str
    picked_up_at: str | None = None


class PendingAsksResponse(BaseModel):
    asks: list[PendingAsk]
    current_turn_seq: int


@router.post("/ask", response_model=AskResponse)
async def open_ask(
    request: AskRequest,
    _: str | None = Depends(require_auth),
) -> AskResponse:
    """Open a non-blocking ask. Returns corr_id immediately."""
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker
    await peer_registry.lazy_repair()

    # Resolve target peer
    peer = await peer_registry.get_peer(request.to_peer, circle=request.circle)
    if not peer:
        raise HTTPException(status_code=404, detail=f"Unknown peer: {request.to_peer}")

    # Resolve sender (best-effort — sender may be a CLI/external caller)
    from_peer_obj = await peer_registry.get_peer(request.from_peer)
    from_peer_id = from_peer_obj.peer_id if from_peer_obj else request.from_peer

    # If reply_to is set, close that ask first as 'reply_to'
    if request.reply_to:
        prior = await ask_tracker.close(request.reply_to, reason="reply_to")
        if prior is None:
            logger.debug(
                "ask reply_to=%s: prior ask not found or already closed",
                request.reply_to,
            )

    cid = await ask_tracker.register(
        from_peer_id=from_peer_id,
        from_peer_name=request.from_peer,
        to_peer_id=peer.peer_id,
        to_peer_name=peer.display_name,
        text=request.text,
        reply_to=request.reply_to,
    )

    # Deliver as a first-class type=ask wire message. The recipient transport
    # (ws-hook / opencode plugin / channel server) POSTs /asks/{cid}/picked_up
    # after presenting the message to its agent.
    try:
        await peer_registry.deliver_ask(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            text=request.text,
            correlation_id=cid,
            reply_to=request.reply_to,
            bypass_circle=request.bypass_circle,
            circle=request.circle,
        )
    except ValueError as e:
        # Peer not found by registry — close the ask we just opened
        await ask_tracker.close(cid, reason="evicted")
        raise HTTPException(status_code=404, detail=str(e))

    delivery_status = "queued" if peer.status == PeerStatus.BUSY else "sent"
    return AskResponse(correlation_id=cid, status=delivery_status)


@router.post("/ack", response_model=OkResponse)
async def ack_ask(
    request: AckRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Close an ask. With `message`, delivers the reply to the original asker."""
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker

    reason = "ack_with_msg" if request.message else "ack"
    closed = await ask_tracker.close(request.correlation_id, reason=reason)
    if closed is None:
        # Either unknown corr_id or already closed. Distinguish via get():
        # idempotent re-ack returns 200, truly unknown returns 404.
        if await ask_tracker.get(request.correlation_id) is None:
            raise HTTPException(
                status_code=404,
                detail=f"No open ask with correlation_id: {request.correlation_id}",
            )
        return OkResponse()

    if request.message:
        # bypass_circle=True: ack closes a thread already established at
        # ask-time; circle gate doesn't reapply.
        framed = f"[ack #{request.correlation_id} from @{request.from_peer}] {request.message}"
        try:
            await peer_registry.notify(
                from_peer=request.from_peer,
                to_peer=closed.from_peer_name,
                text=framed,
                bypass_circle=True,
            )
        except ValueError as e:
            logger.warning(
                "ack reply delivery failed for %s: %s", request.correlation_id, e,
            )
        except Exception as e:
            logger.exception(
                "ack reply delivery error for %s: %s", request.correlation_id, e,
            )

    return OkResponse()


@router.post("/asks/{correlation_id}/picked_up", response_model=OkResponse)
async def mark_picked_up(
    correlation_id: str,
    request: PickedUpRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Stop hook records that an ask was picked up at a given turn boundary."""
    state = get_app_state()
    ask_tracker = state.ask_tracker
    if request.correlation_id != correlation_id:
        raise HTTPException(status_code=400, detail="correlation_id mismatch")
    await ask_tracker.mark_picked_up(correlation_id)
    return OkResponse()


@router.post("/asks/{correlation_id}/mark_reminded", response_model=OkResponse)
async def mark_reminded(
    correlation_id: str,
    request: MarkRemindedRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Flip the once-only reminded flag after the Stop hook injects a reminder."""
    state = get_app_state()
    ask_tracker = state.ask_tracker
    if request.correlation_id != correlation_id:
        raise HTTPException(status_code=400, detail="correlation_id mismatch")
    await ask_tracker.mark_reminded(correlation_id)
    return OkResponse()


@router.get("/asks/pending", response_model=PendingAsksResponse)
async def pending_asks(
    pane_id: str,
    _: str | None = Depends(require_auth),
) -> PendingAsksResponse:
    """Return asks targeting this pane that are due for a reminder.

    Bumps the per-peer turn counter as a side effect (each Stop hook call =
    one turn). Returns up to 3 most-recent picked-up-but-not-reminded asks
    that have aged past the one-turn grace window.
    """
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker

    peer = await peer_registry.get_peer_by_pane(pane_id)
    if not peer:
        raise HTTPException(status_code=404, detail=f"No peer for pane: {pane_id}")

    current_turn = await ask_tracker.increment_turn(peer.peer_id)
    pending = await ask_tracker.pending_for_peer(peer.peer_id, current_turn)

    return PendingAsksResponse(
        asks=[
            PendingAsk(
                correlation_id=ask.correlation_id,
                from_peer=ask.from_peer_name,
                text=ask.text,
                created_at=ask.created_at.isoformat(),
                picked_up_at=ask.picked_up_at.isoformat() if ask.picked_up_at else None,
            )
            for ask in pending
        ],
        current_turn_seq=current_turn,
    )
