"""Ask/ack lifecycle endpoints.

The non-blocking ask/ack model:

  POST /ask                       — register an ask, inject to recipient, return corr_id
  POST /ack                       — close an ask (bare or with reply content)
  GET  /asks/pending              — recipient's Stop hook polls for open asks

The wire protocol carries asks to peers as a first-class `type: ask`
message: `{type: ask, correlation_id, from_peer, text, reply_to}`. Each
transport (ws-hook, opencode plugin, channel server) dispatches
`type=ask` and the recipient agent acks via the `ack` MCP tool.

Open asks are surfaced on every Stop hook poll until acked — no
once-only reminder, no turn-counter grace window. The agent is free to
ignore a reminder; it'll show up again next turn.

`POST /asks/{cid}/picked_up` and `POST /asks/{cid}/mark_reminded` are
kept as silent no-op 200 endpoints for one release so older transports
(channel server, opencode plugin installs) don't see 404 noise during
the upgrade.
"""

from __future__ import annotations

import logging

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.routes._shared import OkResponse
from repowire.daemon.websocket_transport import TransportError

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
    error: str | None = None


class AckRequest(BaseModel):
    """Close an ask. If `message` is set, IS the reply (delivered to asker)."""

    correlation_id: str
    message: str | None = None
    from_peer: str = Field(..., description="Display name of the acking peer")


class _NoOpRequest(BaseModel):
    """Compat shim: legacy clients may POST a body to deprecated endpoints."""

    correlation_id: str | None = None


class PendingAsk(BaseModel):
    correlation_id: str
    from_peer: str
    text: str
    created_at: str


class PendingAsksResponse(BaseModel):
    asks: list[PendingAsk]


@router.post("/ask", response_model=AskResponse)
async def open_ask(
    request: AskRequest,
    _: str | None = Depends(require_auth),
) -> AskResponse:
    """Open a non-blocking ask.

    Pre-registers in the tracker, attempts wire send. On TransportError the
    newly-registered ask is closed (rollback) and 503 is returned so the
    caller can retry when the recipient is back online.
    """
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker
    await peer_registry.lazy_repair()

    peer = await peer_registry.get_peer(request.to_peer, circle=request.circle)
    if not peer:
        raise HTTPException(status_code=404, detail=f"Unknown peer: {request.to_peer}")

    from_peer_obj = await peer_registry.get_peer(request.from_peer)
    from_peer_id = from_peer_obj.peer_id if from_peer_obj else request.from_peer

    cid = await ask_tracker.register(
        from_peer_id=from_peer_id,
        from_peer_name=request.from_peer,
        to_peer_id=peer.peer_id,
        to_peer_name=peer.display_name,
        text=request.text,
        reply_to=request.reply_to,
    )

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
        await ask_tracker.close(cid, reason="evicted")
        raise HTTPException(status_code=404, detail=str(e))
    except TransportError as e:
        await ask_tracker.close(cid, reason="send_failed")
        raise HTTPException(
            status_code=503,
            detail=f"Peer {request.to_peer} has no live connection: {e}",
        )

    # Send succeeded: close any prior thread referenced by reply_to.
    if request.reply_to:
        prior = await ask_tracker.close(request.reply_to, reason="reply_to")
        if prior is None:
            logger.debug(
                "ask reply_to=%s: prior ask not found or already closed",
                request.reply_to,
            )

    return AskResponse(correlation_id=cid)


@router.post("/ack", response_model=OkResponse)
async def ack_ask(
    request: AckRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Close an ask. With `message`, delivers the reply to the original asker.

    Bare ack: close the ask, return 200.

    Ack-with-message: deliver the reply first; only close on successful
    delivery. If the asker has no live WS the ask stays open and 503 is
    returned so the recipient can retry (or drop the message and bare-ack
    if they give up). This avoids closing the thread while silently dropping
    the reply under the new fail-loud / no-queue contract.

    Returns:
        200 on success, 200 on idempotent re-ack (already closed), 404 if
        unknown corr_id, 503 if reply delivery failed.
    """
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker

    existing = await ask_tracker.get(request.correlation_id)
    if existing is None:
        raise HTTPException(
            status_code=404,
            detail=f"No open ask with correlation_id: {request.correlation_id}",
        )
    if existing.closed:
        # Idempotent re-ack: already closed, nothing to do.
        return OkResponse()

    if request.message:
        # bypass_circle=True: ack closes a thread already established at
        # ask-time; circle gate doesn't reapply.
        framed = f"[ack #{request.correlation_id} from @{request.from_peer}] {request.message}"
        try:
            await peer_registry.notify(
                from_peer=request.from_peer,
                to_peer=existing.from_peer_name,
                text=framed,
                bypass_circle=True,
            )
        except ValueError as e:
            # Asker peer no longer in registry. Close as best-effort and
            # log; nothing to retry against.
            logger.warning(
                "ack reply for %s: asker missing (%s); closing without delivery",
                request.correlation_id, e,
            )
            await ask_tracker.close(request.correlation_id, reason="ack_with_msg")
            return OkResponse()
        except TransportError as e:
            # Asker has no live WS. Leave the ask open so the recipient can
            # retry (and report 503 so the MCP caller knows the reply did
            # not land).
            raise HTTPException(
                status_code=503,
                detail=(
                    f"Reply delivery failed for {existing.from_peer_name}: {e}. "
                    "Ask remains open; retry when the asker reconnects."
                ),
            )
        except Exception as e:
            # Unexpected error — also leave the ask open and surface a 500.
            logger.exception(
                "ack reply delivery error for %s: %s", request.correlation_id, e,
            )
            raise HTTPException(
                status_code=500,
                detail=f"Reply delivery error: {e}",
            )

        await ask_tracker.close(request.correlation_id, reason="ack_with_msg")
    else:
        await ask_tracker.close(request.correlation_id, reason="ack")

    return OkResponse()


@router.post("/asks/{correlation_id}/picked_up", response_model=OkResponse)
async def mark_picked_up(
    correlation_id: str,  # noqa: ARG001
    request: _NoOpRequest,  # noqa: ARG001
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Deprecated no-op kept for transport-compat during one release.

    Old ws-hook / channel server / opencode plugin installs POST here after
    delivering a type=ask. Under the simplified model the daemon no longer
    tracks pickup state — reminders fire on every Stop hook for any open ask.
    """
    return OkResponse()


@router.post("/asks/{correlation_id}/mark_reminded", response_model=OkResponse)
async def mark_reminded(
    correlation_id: str,  # noqa: ARG001
    request: _NoOpRequest,  # noqa: ARG001
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Deprecated no-op kept for hook-compat during one release.

    Old Stop hooks POSTed here after writing the once-only reminder. Under
    the simplified model open asks reappear in every Stop poll until acked.
    """
    return OkResponse()


@router.get("/asks/pending", response_model=PendingAsksResponse)
async def pending_asks(
    pane_id: str,
    _: str | None = Depends(require_auth),
) -> PendingAsksResponse:
    """Return all open asks targeting this pane's peer, newest first."""
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker

    peer = await peer_registry.get_peer_by_pane(pane_id)
    if not peer:
        raise HTTPException(status_code=404, detail=f"No peer for pane: {pane_id}")

    pending = await ask_tracker.pending_for_peer(peer.peer_id)

    return PendingAsksResponse(
        asks=[
            PendingAsk(
                correlation_id=ask.correlation_id,
                from_peer=ask.from_peer_name,
                text=ask.text,
                created_at=ask.created_at.isoformat(),
            )
            for ask in pending
        ],
    )
