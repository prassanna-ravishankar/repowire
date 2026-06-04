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
from uuid import uuid4

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.daemon.ask_many import (
    DEFAULT_ASK_MANY_TIMEOUT_SECONDS,
    MAX_ASK_MANY_PEERS,
    AskManyChild,
    AskManyTracker,
)
from repowire.daemon.ask_service import (
    AckCommand,
    AnswerCommand,
    AskService,
    AskServiceError,
    OpenAskCommand,
)
from repowire.daemon.ask_tracker import AskerIdentity, AskTracker, QuiescedError
from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state, get_peer_registry
from repowire.daemon.peer_delivery import peer_delivery_from_state
from repowire.daemon.peer_registry import PeerRegistry, normalize_identity_path
from repowire.daemon.question_blocking import register_blocking_question_and_wait
from repowire.daemon.routes._shared import OkResponse
from repowire.daemon.websocket_transport import DeliveryInjectionError, TransportError
from repowire.protocol.messages import AttachmentRef
from repowire.protocol.peers import Peer
from repowire.protocol.questions import (
    Answer,
    AnswerOutcome,
    Question,
    QuestionOption,
    QuestionScope,
)

logger = logging.getLogger(__name__)
router = APIRouter(tags=["asks"])


def _ask_service() -> AskService:
    return AskService(state=get_app_state(), peer_registry=get_peer_registry())


def _raise_http(error: AskServiceError) -> None:
    raise HTTPException(status_code=error.status_code, detail=error.detail)


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
    question: Question | None = Field(
        None,
        description=(
            "Optional structured question (approval / choice / text). When set, "
            "the recipient answers via /answer (or ack); the answer is typed."
        ),
    )


class AskResponse(BaseModel):
    """Result of opening an ask."""

    correlation_id: str
    error: str | None = None


class AnswerRequest(BaseModel):
    """Answer a question ask (or a plain ask) with a typed Answer.

    Canonical endpoint for the mesh-questions primitive. ``/ack`` delegates here.
    """

    correlation_id: str
    option_id: str | None = Field(None, description="Selected option for a choice question")
    text: str | None = Field(None, description="Free-text answer / reply body")
    outcome: AnswerOutcome = Field("answered")
    message: str | None = Field(None, description="Optional human note alongside the answer")
    attachments: list[AttachmentRef] = Field(default_factory=list)


# Hard server-side cap on how long /questions/ask-blocking holds a connection
# open waiting for an answer. A blocking transport's client timeout must sit
# above this (+ network margin) but below its own host timeout (e.g. Claude's
# PreToolUse hook timeout). Timeout always records the question's default_answer
# (fail-closed for tool_permission), so the caller never hangs.
BLOCKING_QUESTION_MAX_WAIT_SECONDS = 55.0
BLOCKING_QUESTION_DEFAULT_WAIT_SECONDS = 45.0


class BlockingQuestionOption(BaseModel):
    """An answerable option on a blocking question."""

    id: str = Field(..., description="Stable option id the answer selects")
    title: str = Field(..., description="Human-readable label")


class AskBlockingRequest(BaseModel):
    """Post a blocking structured question from an external transport.

    Transport-neutral: an ACP runtime, a Claude Code PreToolUse hook, or any
    client that owns a suspendable call registers a question, the daemon holds
    the connection open up to ``BLOCKING_QUESTION_MAX_WAIT_SECONDS`` while a
    human surface (dashboard / Telegram) or a peer answers, and the typed
    answer is returned. Fail-closed: on timeout the question's default answer is
    recorded.
    """

    prompt: str = Field(..., description="Short human prompt for the question")
    options: list[BlockingQuestionOption] = Field(
        default_factory=list,
        description="Allow-capable options; at least one required for tool_permission",
    )
    scope: QuestionScope | None = Field(
        None, description="Question scope, e.g. 'tool_permission'",
    )
    metadata: dict | None = Field(
        None, description="Structured (truncated) context for renderers / audit",
    )
    timeout_seconds: float | None = Field(
        None, description="Requested wait; clamped to the daemon max",
    )
    correlation_id: str | None = Field(
        None,
        description="Caller-supplied cid for idempotent retries (e.g. 'pretool-<hex>')",
    )
    origin: str | None = Field(
        None, description="Originating transport label (e.g. 'pretooluse', 'acp')",
    )
    from_peer: str | None = Field(
        None, description="Display name / id of the peer the question is about",
    )


class AskBlockingResponse(BaseModel):
    """Resolved answer for a blocking question."""

    correlation_id: str
    outcome: AnswerOutcome
    option_id: str | None = None
    message: str | None = None


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
    # Reply delivered to the asker: capture the body for ask-many aggregation
    # (no-op for non-child asks). Only here — the TransportError/ValueError/error
    # paths above must NOT mark a child replied without proven delivery.
    if not is_error and reply is not None:
        await ask_tracker.capture_child_reply(correlation_id, reply)
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


def _make_on_acp_complete(ask_tracker: AskTracker, peer_registry: PeerRegistry):
    """Build the ACP completion callback used for ask + ask-many children.

    Delegates to the shared _acp_complete, which captures the ask-many child
    reply only after the framed reply is delivered to the asker (so a failed
    delivery leaves the child pending, not replied).
    """

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

    return _on_acp_complete


@router.post("/ask", response_model=AskResponse)
async def open_ask(
    request: AskRequest,
    _: str | None = Depends(require_auth),
) -> AskResponse:
    """Open a non-blocking ask."""
    try:
        result = await _ask_service().open_ask(
            OpenAskCommand(
                from_peer=request.from_peer,
                to_peer=request.to_peer,
                text=request.text,
                attachments=request.attachments,
                reply_to=request.reply_to,
                bypass_circle=request.bypass_circle,
                circle=request.circle,
                question=request.question,
            )
        )
    except AskServiceError as e:
        _raise_http(e)
    return AskResponse(correlation_id=result.correlation_id)


class AskManyRequest(BaseModel):
    """Open a parent ask-many fanning out to a bounded peer list."""

    from_peer: str = Field(..., description="Display name of the sender")
    to_peers: list[str] = Field(..., description="Recipient display names (deduped)")
    text: str = Field(..., description="Ask content sent to every recipient")
    circle: str | None = Field(default=None)
    bypass_circle: bool = Field(default=False)
    timeout_seconds: int = Field(
        default=DEFAULT_ASK_MANY_TIMEOUT_SECONDS,
        description="Lazy deadline for partial/timeout reporting (no active timer)",
    )


class AskManyChildResponse(BaseModel):
    peer: str
    correlation_id: str | None = None
    delivered: bool = False
    error: str | None = None


class AskManyResponse(BaseModel):
    parent_id: str
    children: list[AskManyChildResponse]


@router.post("/ask-many", response_model=AskManyResponse)
async def open_ask_many(
    request: AskManyRequest,
    _: str | None = Depends(require_auth),
) -> AskManyResponse:
    """Fan out one ask to many peers under a parent id, best-effort per child.

    Each child is a normal ask (acked via the usual /ack); the parent is a
    tracking anchor only. A child that fails to resolve/deliver is recorded as
    a failed child and does not abort the rest. v1: no quorum/retry/map-reduce.
    """
    peer_registry = get_peer_registry()
    state = get_app_state()
    ask_tracker: AskTracker = state.ask_tracker
    ask_many: AskManyTracker = state.ask_many_tracker
    await peer_registry.lazy_repair()

    if not request.to_peers:
        raise HTTPException(status_code=422, detail="to_peers must not be empty")
    if len(request.to_peers) > MAX_ASK_MANY_PEERS:
        raise HTTPException(
            status_code=422,
            detail=f"to_peers exceeds the {MAX_ASK_MANY_PEERS}-peer limit",
        )

    parent = await ask_many.create(
        from_peer_id=None,
        from_peer_name=request.from_peer,
        text=request.text,
        timeout_seconds=request.timeout_seconds,
    )

    peer_delivery = peer_delivery_from_state(
        config=state.config, registry=peer_registry, state=state,
    )

    children: list[AskManyChildResponse] = []
    seen_peer_ids: set[str] = set()
    for to_peer in dict.fromkeys(request.to_peers):  # de-dup by display name
        try:
            peer = await peer_registry.get_peer(to_peer, circle=request.circle)
        except ValueError as e:
            peer = None
            resolve_error = str(e)
        else:
            resolve_error = None if peer else f"peer not found: {to_peer}"
        if peer is None:
            await ask_many.add_child(
                parent.parent_id, AskManyChild(peer_name=to_peer, delivery_error=resolve_error),
            )
            children.append(AskManyChildResponse(peer=to_peer, error=resolve_error))
            continue
        if peer.peer_id in seen_peer_ids:
            continue  # de-dup peers that resolve to the same identity
        seen_peer_ids.add(peer.peer_id)

        from_obj = await _resolve_sender_for_target(request.from_peer, peer)
        from_peer_id = from_obj.peer_id if from_obj else request.from_peer
        from_peer_name = from_obj.display_name if from_obj else request.from_peer
        cid = await ask_tracker.register(
            from_peer_id=from_peer_id,
            from_peer_name=from_peer_name,
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text=request.text,
            parent_id=parent.parent_id,
        )
        try:
            await peer_delivery.deliver_ask(
                from_peer=from_peer_id,
                to_peer=peer.peer_id,
                text=request.text,
                correlation_id=cid,
                bypass_circle=request.bypass_circle,
                on_acp_complete=_make_on_acp_complete(ask_tracker, peer_registry),
            )
            delivered = True
            err = None
        except (TransportError, DeliveryInjectionError, ValueError) as e:
            await ask_tracker.close(cid, reason="send_failed")
            delivered = False
            err = str(e)
        await ask_many.add_child(
            parent.parent_id,
            AskManyChild(
                peer_name=peer.display_name, correlation_id=cid, peer_id=peer.peer_id,
                delivery_error=err,
            ),
        )
        children.append(
            AskManyChildResponse(
                peer=peer.display_name, correlation_id=cid, delivered=delivered, error=err,
            )
        )

    return AskManyResponse(parent_id=parent.parent_id, children=children)


@router.get("/ask-many/{parent_id}")
async def ask_many_result(
    parent_id: str,
    _: str | None = Depends(require_auth),
) -> dict:
    """Aggregate an ask-many parent's current state from its children."""
    state = get_app_state()
    ask_many: AskManyTracker = state.ask_many_tracker
    result = await ask_many.status(parent_id)
    if result is None:
        raise HTTPException(status_code=404, detail=f"ask-many {parent_id} not found")
    return result


@router.post("/questions/ask-blocking", response_model=AskBlockingResponse)
async def ask_blocking_question(
    request: AskBlockingRequest,
    _: str | None = Depends(require_auth),
) -> AskBlockingResponse:
    """Register a blocking structured question and wait for its answer.

    The daemon holds this connection open up to a hard cap while a human surface
    (dashboard / Telegram) or a peer answers. Fail-closed: a tool-permission
    question denies on timeout / acknowledge / no-option (its default answer is
    recorded through the answer machinery, so first-answer-wins still holds and
    a late /answer returns 410).

    Returns 422 when a tool_permission question carries no allow-capable option
    (every answer would deny and the UI would look broken).
    """
    if request.scope == "tool_permission" and not request.options:
        raise HTTPException(
            status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail="tool_permission question requires at least one allow-capable option",
        )

    state = get_app_state()
    ask_tracker = state.ask_tracker
    emit_event = get_peer_registry().add_event

    server_wait = min(
        request.timeout_seconds or BLOCKING_QUESTION_DEFAULT_WAIT_SECONDS,
        BLOCKING_QUESTION_MAX_WAIT_SECONDS,
    )
    correlation_id = request.correlation_id or f"ask-{uuid4().hex[:8]}"
    from_peer = request.from_peer or "external"

    # Fail-closed default: tool_permission denies, other scopes time out.
    default_outcome: AnswerOutcome = (
        "denied" if request.scope == "tool_permission" else "timed_out"
    )
    question = Question(
        kind="choice",
        prompt=request.prompt,
        options=[QuestionOption(id=o.id, title=o.title) for o in request.options],
        blocking=True,
        timeout_seconds=server_wait,
        default_answer=Answer(
            outcome=default_outcome, message="blocking question timed out",
        ),
        scope=request.scope,
        metadata=request.metadata or {},
    )
    try:
        answer = await register_blocking_question_and_wait(
            ask_tracker,
            emit_event,
            question=question,
            text=request.prompt,
            correlation_id=correlation_id,
            server_wait_seconds=server_wait,
            from_peer_id=from_peer,
            from_peer_name=from_peer,
            extra_ask_fields={"origin": request.origin} if request.origin else None,
        )
    except QuiescedError as e:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(e)) from e
    return AskBlockingResponse(
        correlation_id=correlation_id,
        outcome=answer.outcome,
        option_id=answer.option_id,
        message=answer.message,
    )


@router.post("/answer", response_model=OkResponse)
async def answer_question(
    request: AnswerRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Answer a structured question with a typed Answer — the canonical verb.

    Records the answer (resolving any blocking transport's waiter), then
    best-effort delivers a human-readable form back to the asker. Returns 404
    (unknown), 410 (already answered/closed), 422 (plain ask — use /ack, or
    invalid option for the question).
    """
    try:
        await _ask_service().answer(
            AnswerCommand(
                correlation_id=request.correlation_id,
                option_id=request.option_id,
                text=request.text,
                outcome=request.outcome,
                message=request.message,
                attachments=request.attachments,
            )
        )
    except AskServiceError as e:
        _raise_http(e)
    return OkResponse()


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
    try:
        await _ask_service().ack(
            AckCommand(
                correlation_id=request.correlation_id,
                message=request.message,
                attachments=request.attachments,
                from_peer=request.from_peer,
            )
        )
    except AskServiceError as e:
        _raise_http(e)
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
