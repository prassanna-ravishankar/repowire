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

from repowire.daemon.ask_tracker import AskerIdentity, AskTracker, QuiescedError
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.peer_delivery import peer_delivery_from_state
from repowire.daemon.peer_registry import PeerRegistry, normalize_identity_path
from repowire.daemon.routes._shared import OkResponse
from repowire.daemon.state.session_bindings import resolve_repowire_session_id
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.messages import AttachmentRef
from repowire.protocol.peers import Peer

logger = logging.getLogger(__name__)
router = APIRouter(tags=["asks"])


class AskRequest(BaseModel):
    """Open a new ask thread."""

    from_peer: str = Field(..., description="Display name of the sender")
    to_peer: str = Field(..., description="Display name of the recipient")
    text: str = Field(..., description="Ask content")
    attachments: list[AttachmentRef] = Field(
        default_factory=list,
        description="Optional daemon attachment metadata to carry with the ask",
    )
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
    attachments: list[AttachmentRef] = Field(
        default_factory=list,
        description="Optional daemon attachment metadata to carry with an ack reply",
    )
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


class PendingDelivery(BaseModel):
    delivery_id: str
    kind: str
    from_peer: str
    to_peer: str
    text: str
    created_at: str
    expires_at: str
    correlation_id: str | None = None
    attachments: list[dict] = Field(default_factory=list)


class PendingDeliveriesResponse(BaseModel):
    deliveries: list[PendingDelivery]


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


async def _capture_asker_identity(
    peer_registry: PeerRegistry, asker_peer_id: str,
) -> AskerIdentity | None:
    """Snapshot the asker's full stable identity for stash-time rebind.

    Returns None unless every field is present and non-default:
      * display_name, circle, backend all non-empty
      * path non-empty after normalization
      * machine non-empty AND not "unknown"

    Partial identity is deliberately refused — misrouting an ACP answer to
    the wrong peer on rebind is worse than non-delivery + visible event.
    """
    try:
        peer = await peer_registry.get_peer(asker_peer_id)
    except ValueError:
        return None
    if peer is None:
        return None
    if not (peer.display_name and peer.circle and peer.backend):
        return None
    if not peer.machine or peer.machine == "unknown":
        return None
    raw_path = peer.path or ""
    norm_path = normalize_identity_path(raw_path)
    if not norm_path:
        return None
    return AskerIdentity(
        display_name=peer.display_name,
        circle=peer.circle,
        backend=peer.backend.value,
        path=norm_path,
        machine=peer.machine,
    )


def _binding_id_for_peer(state: object, peer: Peer | None) -> str | None:
    return resolve_repowire_session_id(
        getattr(state, "session_binding_store", None),
        peer=peer,
    )


def _uses_cli_polling_fallback(peer: Peer) -> bool:
    """Whether asks can be opened for a peer that polls /asks/pending itself."""
    return bool(peer.metadata.get("repowire_cli_fallback"))


def _dump_attachments(attachments: list[AttachmentRef]) -> list[dict]:
    return [a.model_dump(exclude_none=True) for a in attachments]


def _emit_ack_event(
    *,
    peer_registry: PeerRegistry,
    ask: object,
    reason: str,
    delivered: bool,
    has_message: bool,
    has_attachments: bool,
) -> None:
    from repowire.daemon.ask_tracker import Ask

    assert isinstance(ask, Ask)
    peer_registry.add_event(
        "ack",
        {
            "from": ask.to_peer_name,
            "to": ask.from_peer_name,
            "from_peer_id": ask.to_peer_id,
            "to_peer_id": ask.from_peer_id,
            "correlation_id": ask.correlation_id,
            "status": reason,
            "delivered": delivered,
            "has_message": has_message,
            "has_attachments": has_attachments,
            "repowire_session_id": ask.from_repowire_session_id,
            "from_repowire_session_id": ask.to_repowire_session_id,
            "to_repowire_session_id": ask.from_repowire_session_id,
        },
    )


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
            _emit_ack_event(
                peer_registry=peer_registry,
                ask=ask,
                reason="send_failed",
                delivered=False,
                has_message=True,
                has_attachments=False,
            )
            return
        identity = await _capture_asker_identity(peer_registry, ask.from_peer_id)
        stashed = await ask_tracker.set_pending_reply(
            correlation_id, framed, identity=identity,
        )
        logger.warning(
            "ACP ack reply for cid=%s: asker offline (%s). %s%s; "
            "will redeliver on reconnect.",
            correlation_id, e,
            "Reply stashed" if stashed else "Stash failed (ask closed)",
            " with identity tuple" if (stashed and identity) else "",
        )
        return
    except ValueError as e:
        logger.warning(
            "ACP ack reply for cid=%s: asker missing (%s). Closing.",
            correlation_id, e,
        )
        await ask_tracker.close(correlation_id, reason="ack_with_msg")
        _emit_ack_event(
            peer_registry=peer_registry,
            ask=ask,
            reason="ack_with_msg",
            delivered=False,
            has_message=not is_error,
            has_attachments=False,
        )
        return
    await ask_tracker.close(
        correlation_id, reason="ack_with_msg" if not is_error else "send_failed",
    )
    _emit_ack_event(
        peer_registry=peer_registry,
        ask=ask,
        reason="ack_with_msg" if not is_error else "send_failed",
        delivered=True,
        has_message=not is_error,
        has_attachments=False,
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
    trace = getattr(state, "delivery_trace_store", None)
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
    from_repowire_session_id = _binding_id_for_peer(state, from_peer_obj)
    to_repowire_session_id = _binding_id_for_peer(state, peer)

    # Enforce circle access BEFORE registering the ask. On the WS path
    # ``deliver_ask`` runs the same check inside ``_resolve_from_peer_unlocked``
    # and a violation rolls the tracker registration back; the ACP branch
    # never reaches ``deliver_ask``, so we front-load the check via the
    # public ``check_circle_access`` helper so both transports apply the
    # same gate.
    try:
        await peer_registry.check_circle_access(
            from_peer_obj, peer, bypass_circle=request.bypass_circle,
        )
    except ValueError as e:
        raise HTTPException(status_code=403, detail=str(e)) from e

    try:
        cid = await ask_tracker.register(
            from_peer_id=from_peer_id,
            from_peer_name=from_peer_name,
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text=request.text,
            reply_to=request.reply_to,
            from_repowire_session_id=from_repowire_session_id,
            to_repowire_session_id=to_repowire_session_id,
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

    if trace is not None:
        trace.record(
            trace_id=cid, kind="ask", stage="created",
            peer_id=peer.peer_id, from_peer_id=from_peer_id,
        )
        trace.record(
            trace_id=cid, kind="ask", stage="resolved_peer",
            peer_id=peer.peer_id, from_peer_id=from_peer_id,
        )

    async def _on_acp_complete(
        correlation_id: str, reply: str | None, error: str | None,
    ) -> None:
        await _acp_complete(
            correlation_id=correlation_id,
            reply=reply,
            error=error,
            ask_tracker=ask_tracker,
            peer_registry=peer_registry,
        )

    try:
        peer_delivery = peer_delivery_from_state(
            config=state.config,
            registry=peer_registry,
            state=state,
        )
        if trace is not None:
            trace.record(
                trace_id=cid, kind="ask", stage="routed",
                peer_id=peer.peer_id, from_peer_id=from_peer_id,
            )
        await peer_delivery.deliver_ask(
            from_peer=from_peer_id,
            to_peer=peer.peer_id,
            text=request.text,
            correlation_id=cid,
            reply_to=request.reply_to,
            bypass_circle=True,
            attachments=request.attachments,
            on_acp_complete=_on_acp_complete,
        )
    except ValueError as e:
        if trace is not None:
            trace.record(
                trace_id=cid, kind="ask", stage="resolve_failed", status="fail",
                peer_id=peer.peer_id, detail={"error": str(e)},
            )
        await ask_tracker.close(cid, reason="evicted")
        raise HTTPException(status_code=404, detail=str(e))
    except TransportError as e:
        if _uses_cli_polling_fallback(peer):
            queue = getattr(state, "queued_delivery_store", None)
            if queue is not None:
                queue.enqueue(
                    peer_id=peer.peer_id,
                    repowire_session_id=to_repowire_session_id,
                    kind="ask",
                    from_peer_id=from_peer_id,
                    from_peer_name=from_peer_name,
                    to_peer_name=peer.display_name,
                    correlation_id=cid,
                    text=request.text,
                    attachments=_dump_attachments(request.attachments),
                )
            logger.info(
                "Ask %s queued for CLI-polling peer %s (%s): %s",
                cid,
                peer.display_name,
                peer.peer_id,
                e,
            )
            if trace is not None:
                trace.record(
                    trace_id=cid, kind="ask", stage="pending",
                    peer_id=peer.peer_id, detail={"transport": "cli_polling_queue"},
                )
            if request.reply_to:
                prior = await ask_tracker.close(request.reply_to, reason="reply_to")
                if prior is None:
                    logger.debug(
                        "ask reply_to=%s: prior ask not found or already closed",
                        request.reply_to,
                    )
            return AskResponse(correlation_id=cid)
        if trace is not None:
            trace.record(
                trace_id=cid, kind="ask", stage="no_connection", status="fail",
                peer_id=peer.peer_id, detail={"error": str(e)},
            )
        await ask_tracker.close(cid, reason="send_failed")
        raise HTTPException(
            status_code=503,
            detail=f"Peer {request.to_peer} has no live connection: {e}",
        )

    # Send succeeded: the transport completed the wire send and (for ws peers)
    # the ws-hook delivery_ack resolved inside deliver_ask.
    if trace is not None:
        trace.record(
            trace_id=cid, kind="ask", stage="websocket_sent", peer_id=peer.peer_id,
        )
        trace.record(
            trace_id=cid, kind="ask", stage="hook_received", peer_id=peer.peer_id,
        )
        trace.record(
            trace_id=cid, kind="ask", stage="pane_injected", peer_id=peer.peer_id,
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
    trace = getattr(state, "delivery_trace_store", None)

    existing = await ask_tracker.get(request.correlation_id)
    if existing is None:
        raise HTTPException(
            status_code=404,
            detail=f"No open ask with correlation_id: {request.correlation_id}",
        )
    if existing.closed and (request.message or request.attachments):
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

    if request.message or request.attachments:
        # bypass_circle=True: ack closes a thread already established at
        # ask-time; circle gate doesn't reapply.
        framed = (
            f"[ack #{request.correlation_id} from @{existing.to_peer_name}] "
            f"{request.message or ''}"
        )
        try:
            peer_delivery = peer_delivery_from_state(
                config=state.config,
                registry=peer_registry,
                state=state,
            )
            await peer_delivery.notify(
                from_peer=existing.to_peer_id,
                to_peer=existing.from_peer_id,
                text=framed,
                bypass_circle=True,
                attachments=request.attachments,
            )
        except ValueError as e:
            # Asker peer no longer in registry. Close as best-effort and
            # log; nothing to retry against.
            logger.warning(
                "ack reply for %s: asker missing (%s); closing without delivery",
                request.correlation_id, e,
            )
            await ask_tracker.close(request.correlation_id, reason="ack_with_msg")
            _emit_ack_event(
                peer_registry=peer_registry,
                ask=existing,
                reason="ack_with_msg",
                delivered=False,
                has_message=True,
                has_attachments=bool(request.attachments),
            )
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
        _emit_ack_event(
            peer_registry=peer_registry,
            ask=existing,
            reason="ack",
            delivered=False,
            has_message=False,
            has_attachments=False,
        )
        await ask_tracker.close(request.correlation_id, reason="ack")

    if trace is not None:
        trace.record(
            trace_id=request.correlation_id, kind="ask", stage="acked",
            peer_id=existing.to_peer_id, from_peer_id=existing.from_peer_id,
        )
        trace.record(
            trace_id=request.correlation_id, kind="ask", stage="closed",
            peer_id=existing.to_peer_id, from_peer_id=existing.from_peer_id,
        )

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


@router.get("/deliveries/pending", response_model=PendingDeliveriesResponse)
async def pending_deliveries(
    pane_id: str | None = None,
    peer_id: str | None = None,
    max_results: int = 50,
    _: str | None = Depends(require_auth),
) -> PendingDeliveriesResponse:
    """Drain queued deliveries for a polling peer.

    This is delete-on-drain: once a delivery is returned to a Stop hook or CLI
    polling client, it is removed from the durable queue so the same notify/ask
    paste is not replayed indefinitely. Ask lifecycle reminders remain governed
    by `/asks/pending` until the ask is acked.
    """
    if not pane_id and not peer_id:
        raise HTTPException(status_code=400, detail="Must provide pane_id or peer_id")
    if pane_id and peer_id:
        raise HTTPException(status_code=400, detail="Provide only one of pane_id or peer_id")

    peer_registry = get_peer_registry()
    state = get_app_state()
    queue = getattr(state, "queued_delivery_store", None)
    if queue is None:
        return PendingDeliveriesResponse(deliveries=[])

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

    drained = queue.drain_for_peer(resolved_peer_id, max_results=max_results)
    return PendingDeliveriesResponse(
        deliveries=[
            PendingDelivery(
                delivery_id=d.delivery_id,
                kind=d.kind,
                from_peer=d.from_peer_name,
                to_peer=d.to_peer_name,
                correlation_id=d.correlation_id,
                text=d.text,
                attachments=d.attachments or [],
                created_at=d.created_at,
                expires_at=d.expires_at,
            )
            for d in drained
        ],
    )
