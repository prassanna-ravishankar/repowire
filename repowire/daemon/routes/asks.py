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

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.acp import (
    AcpClientManager,
    AcpRouteDecision,
    decide_acp_route,
)
from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker, QuiescedError
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.routes._shared import OkResponse
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.peers import Peer

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
    from_peer: str | None = Field(
        None,
        description=(
            "Compatibility-only acking peer identity. Reply routing uses the "
            "stored ask recipient, not this field."
        ),
    )


class _NoOpRequest(BaseModel):
    """Compat shim: legacy clients may POST a body to deprecated endpoints."""

    correlation_id: str | None = None


class PendingAsk(BaseModel):
    correlation_id: str
    from_peer: str
    to_peer: str
    text: str
    created_at: str
    direction: str  # "inbound" or "outbound"


class PendingAsksResponse(BaseModel):
    asks: list[PendingAsk]


async def _get_peer_or_http(identifier: str, *, circle: str | None = None) -> Peer:
    """Resolve a peer id/display name and convert ambiguity to HTTP errors."""
    peer_registry = get_peer_registry()
    try:
        peer = await peer_registry.get_peer(identifier, circle=circle)
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e)) from e
    if not peer:
        raise HTTPException(status_code=404, detail=f"Unknown peer: {identifier}")
    return peer


async def _resolve_sender_for_target(from_peer: str, target: Peer) -> Peer | None:
    """Resolve an ask sender, preferring the target circle for display-name lookups."""
    peer_registry = get_peer_registry()
    try:
        return (
            await peer_registry.get_peer(from_peer, circle=target.circle)
            or await peer_registry.get_peer(from_peer)
        )
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e)) from e


def _maybe_decide_acp_route(
    peer: Peer,
    config: Config,
    manager: AcpClientManager | None,
) -> AcpRouteDecision | None:
    """Return a positive route decision iff ACP routing applies. Else ``None``.

    Keeps the route handler readable: the only fact the caller needs is "use
    ACP for this ask, yes/no". Reasons / diagnostics are logged inside.
    """
    if manager is None:
        return None
    flag = bool(config.experiments.acp_broker_client) if config.experiments else False
    decision = decide_acp_route(peer, flag_enabled=flag)
    if decision.route and decision.spec is not None:
        return decision
    return None


async def _acp_complete(
    *,
    correlation_id: str,
    reply: str | None,
    error: str | None,
    ask_tracker: AskTracker,
    peer_registry: PeerRegistry,
) -> None:
    """Deliver an ACP-routed ask result back to the asker.

    Delivery semantics:

      * Successful ACP turn + successful notify   → close (ack_with_msg)
      * ACP error (crash/timeout/…)               → close (send_failed) with
                                                     an error frame so the
                                                     asker is not silently
                                                     dropped
      * ValueError on notify (asker evicted)      → close; nothing to retry
      * TransportError on notify, **success path**→ stash the assembled reply
                                                     on the ask + leave it
                                                     open. ``PeerRegistry``
                                                     drains stashed replies
                                                     when the asker comes back
                                                     online (see
                                                     ``_redeliver_pending_replies``).
      * TransportError on notify, **error path**  → close (send_failed). The
                                                     ACP error frame is
                                                     ephemeral diagnostic
                                                     text, not a real reply;
                                                     redelivery would just
                                                     spam the asker with a
                                                     stale failure they can't
                                                     act on.

    The stash-and-redeliver path is the ACP analogue of /ack's 503 retry
    contract: /ack returns 503 so the MCP caller retries; ACP has no caller
    to bounce the error to (the originating turn is gone), so the broker
    holds the reply until the asker is reachable.
    """
    ask = await ask_tracker.get(correlation_id)
    if ask is None or ask.closed:
        return
    is_error = error is not None
    if is_error:
        framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] ACP error: {error}"
    else:
        body = reply or ""
        framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] {body}"
    try:
        await peer_registry.notify(
            from_peer=ask.to_peer_id,
            to_peer=ask.from_peer_id,
            text=framed,
            bypass_circle=True,
        )
    except TransportError as e:
        if is_error:
            logger.warning(
                "ACP ack error-frame for cid=%s undeliverable (%s); closing.",
                correlation_id, e,
            )
            await ask_tracker.close(correlation_id, reason="send_failed")
            return
        stashed = await ask_tracker.set_pending_reply(correlation_id, framed)
        logger.warning(
            "ACP ack reply for cid=%s: asker offline (%s). %s; will redeliver on reconnect.",
            correlation_id, e,
            "Reply stashed" if stashed else "Stash failed (ask closed)",
        )
        return
    except ValueError as e:
        logger.warning(
            "ACP ack reply for cid=%s: asker missing (%s). Closing.",
            correlation_id, e,
        )
        await ask_tracker.close(correlation_id, reason="ack_with_msg")
        return
    await ask_tracker.close(
        correlation_id, reason="ack_with_msg" if not is_error else "send_failed",
    )


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

    peer = await _get_peer_or_http(request.to_peer, circle=request.circle)

    from_peer_obj = await _resolve_sender_for_target(request.from_peer, peer)
    if from_peer_obj is None:
        logger.warning(
            "Opening ask with unresolved sender %r for target peer_id=%s name=%s circle=%s",
            request.from_peer,
            peer.peer_id,
            peer.display_name,
            peer.circle,
        )
    from_peer_id = from_peer_obj.peer_id if from_peer_obj else request.from_peer
    from_peer_name = from_peer_obj.display_name if from_peer_obj else request.from_peer

    try:
        cid = await ask_tracker.register(
            from_peer_id=from_peer_id,
            from_peer_name=from_peer_name,
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text=request.text,
            reply_to=request.reply_to,
        )
    except QuiescedError as e:
        raise HTTPException(
            status_code=503,
            detail={
                "error": "peer_switching",
                "hint": f"Peer {request.to_peer} is mid-switch; retry shortly.",
                "peer_id": e.peer_id,
            },
        ) from e

    # ACP routing (experiments.acp_broker_client). When the flag is on and the
    # target peer carries an `acp` metadata block, bypass the WS transport and
    # drive a session/prompt against the peer's ACP subprocess. The reply is
    # delivered back to the asker via the normal ack pipeline once the prompt
    # turn settles.
    cfg = state.config
    acp_manager = getattr(state, "acp_manager", None)
    decision = _maybe_decide_acp_route(peer, cfg, acp_manager)
    if decision is not None:
        from repowire.acp import deliver_ask_via_acp

        assert acp_manager is not None  # narrowed by _maybe_decide_acp_route
        assert decision.spec is not None
        spec = decision.spec
        manager: AcpClientManager = acp_manager

        async def _on_complete(
            correlation_id: str, reply: str | None, error: str | None,
        ) -> None:
            await _acp_complete(
                correlation_id=correlation_id,
                reply=reply,
                error=error,
                ask_tracker=ask_tracker,
                peer_registry=peer_registry,
            )

        await deliver_ask_via_acp(
            manager=manager,
            spec=spec,
            correlation_id=cid,
            text=request.text,
            on_complete=_on_complete,
        )
        if request.reply_to:
            prior = await ask_tracker.close(request.reply_to, reason="reply_to")
            if prior is None:
                logger.debug(
                    "ask reply_to=%s: prior ask not found or already closed",
                    request.reply_to,
                )
        return AskResponse(correlation_id=cid)

    try:
        await peer_registry.deliver_ask(
            from_peer=from_peer_id,
            to_peer=peer.peer_id,
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

    Bare re-ack of an already-closed ask is idempotent and returns 200.

    Ack-with-message: deliver the reply first; only close on successful
    delivery. If the ask is already closed, the reply cannot be delivered and
    410 is returned. If the asker has no live WS the ask stays open and 503 is
    returned so the recipient can retry (or drop the message and bare-ack if
    they give up). This avoids closing the thread while silently dropping the
    reply under the new fail-loud / no-queue contract.

    Returns:
        200 on success, 200 on idempotent bare re-ack, 404 if unknown corr_id,
        410 if an ack-with-message targets an already-closed ask, 503 if reply
        delivery failed.
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
    if existing.closed and request.message:
        raise HTTPException(
            status_code=status.HTTP_410_GONE,
            detail=(
                f"Ask {request.correlation_id} is already closed; "
                "reply message was not delivered."
            ),
        )
    if existing.closed:
        # Idempotent bare re-ack: already closed, nothing to do.
        return OkResponse()

    if request.message:
        # bypass_circle=True: ack closes a thread already established at
        # ask-time; circle gate doesn't reapply.
        framed = f"[ack #{request.correlation_id} from @{existing.to_peer_name}] {request.message}"
        try:
            await peer_registry.notify(
                from_peer=existing.to_peer_id,
                to_peer=existing.from_peer_id,
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
    pane_id: str | None = None,
    peer_id: str | None = None,
    direction: str = "inbound",
    _: str | None = Depends(require_auth),
) -> PendingAsksResponse:
    """Return open asks for this peer, newest first.

    Lookup is by either `pane_id` (tmux-pane-keyed transports: Claude Code,
    Codex, Gemini Stop hooks) or `peer_id` (transports that own multiple
    peers per process, like the pi extension which runs N sessions sharing
    one tmux pane). Exactly one is required.

    `direction` selects which side of the ask thread to return:
      - "inbound"  (default) — asks targeting this peer (Stop-hook reminders)
      - "outbound" — asks this peer opened (used by `repowire peer describe`)
      - "both"    — union of the two

    The default is `inbound` to preserve back-compat with Stop-hook polls.
    """
    if not pane_id and not peer_id:
        raise HTTPException(status_code=400, detail="Must provide pane_id or peer_id")
    if pane_id and peer_id:
        raise HTTPException(status_code=400, detail="Provide only one of pane_id or peer_id")
    if direction not in ("inbound", "outbound", "both"):
        raise HTTPException(
            status_code=400,
            detail="direction must be one of: inbound, outbound, both",
        )

    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker = state.ask_tracker

    if pane_id:
        peer = await peer_registry.get_peer_by_pane(pane_id)
        if not peer:
            raise HTTPException(status_code=404, detail=f"No peer for pane: {pane_id}")
        resolved_peer_id = peer.peer_id
    else:
        assert peer_id is not None
        peer = await peer_registry.get_peer(peer_id)
        if not peer:
            raise HTTPException(status_code=404, detail=f"No peer with id: {peer_id}")
        resolved_peer_id = peer.peer_id

    pending = await ask_tracker.pending_for_peer(resolved_peer_id, direction=direction)

    def _direction_for(ask: object) -> str:
        # Inbound if the ask targets this peer, outbound if from this peer.
        # With direction="both" the same ask can only be one of the two.
        from repowire.daemon.ask_tracker import Ask  # local to avoid cycle in tests
        assert isinstance(ask, Ask)
        return "inbound" if ask.to_peer_id == resolved_peer_id else "outbound"

    return PendingAsksResponse(
        asks=[
            PendingAsk(
                correlation_id=ask.correlation_id,
                from_peer=ask.from_peer_name,
                to_peer=ask.to_peer_name,
                text=ask.text,
                created_at=ask.created_at.isoformat(),
                direction=_direction_for(ask),
            )
            for ask in pending
        ],
    )
