"""Application service for ask/ack flows.

This module owns the non-blocking ask lifecycle that HTTP routes and MCP tools
drive through the daemon API. It deliberately does not cover ask-many fanout or
blocking questions; those remain separate route-level surfaces for now.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any

from repowire.daemon.ask_tracker import Ask, AskerIdentity, AskTracker, QuiescedError
from repowire.daemon.deps import AppState
from repowire.daemon.peer_delivery import peer_delivery_from_state
from repowire.daemon.peer_registry import PeerRegistry, normalize_identity_path
from repowire.daemon.state.session_bindings import resolve_repowire_session_id
from repowire.daemon.websocket_transport import DeliveryInjectionError, TransportError
from repowire.protocol.messages import AttachmentRef
from repowire.protocol.peers import Peer
from repowire.protocol.questions import Answer, AnswerOutcome, Question

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class AskServiceError(Exception):
    """Domain error surfaced by HTTP/MCP adapters."""

    status_code: int
    detail: Any


@dataclass(frozen=True)
class OpenAskCommand:
    from_peer: str
    to_peer: str
    text: str
    attachments: list[AttachmentRef] = field(default_factory=list)
    reply_to: str | None = None
    bypass_circle: bool = False
    circle: str | None = None
    question: Question | None = None


@dataclass(frozen=True)
class AnswerCommand:
    correlation_id: str
    option_id: str | None = None
    text: str | None = None
    outcome: AnswerOutcome = "answered"
    message: str | None = None
    attachments: list[AttachmentRef] = field(default_factory=list)


@dataclass(frozen=True)
class AckCommand:
    correlation_id: str
    message: str | None = None
    attachments: list[AttachmentRef] = field(default_factory=list)
    from_peer: str | None = None


@dataclass(frozen=True)
class OpenAskResult:
    correlation_id: str


def dump_attachments(attachments: list[AttachmentRef]) -> list[dict]:
    return [a.model_dump(exclude_none=True) for a in attachments]


def uses_cli_polling_fallback(peer: Peer) -> bool:
    """Whether asks can be opened for a peer that polls /asks/pending itself."""
    return bool(peer.metadata.get("repowire_cli_fallback"))


class AskService:
    """Coordinates ask open, ack, and structured answer behavior."""

    def __init__(self, *, state: AppState, peer_registry: PeerRegistry) -> None:
        self._state = state
        self._peer_registry = peer_registry
        self._ask_tracker: AskTracker = state.ask_tracker

    async def open_ask(self, command: OpenAskCommand) -> OpenAskResult:
        """Open and deliver a non-blocking ask."""
        peer_registry = self._peer_registry
        state = self._state
        ask_tracker = self._ask_tracker
        trace = getattr(state, "delivery_trace_store", None)
        await peer_registry.lazy_repair()

        peer = await self._get_peer_or_error(command.to_peer, circle=command.circle)
        from_peer_obj = await self._resolve_sender_for_target(command.from_peer, peer)
        if from_peer_obj is None:
            logger.warning(
                "Opening ask with unresolved sender %r for target peer_id=%s name=%s circle=%s",
                command.from_peer,
                peer.peer_id,
                peer.display_name,
                peer.circle,
            )
        from_peer_id = from_peer_obj.peer_id if from_peer_obj else command.from_peer
        from_peer_name = from_peer_obj.display_name if from_peer_obj else command.from_peer
        from_repowire_session_id = self._binding_id_for_peer(from_peer_obj)
        to_repowire_session_id = self._binding_id_for_peer(peer)

        try:
            await peer_registry.check_circle_access(
                from_peer_obj, peer, bypass_circle=command.bypass_circle,
            )
        except ValueError as e:
            raise AskServiceError(403, str(e)) from e

        try:
            cid = await ask_tracker.register(
                from_peer_id=from_peer_id,
                from_peer_name=from_peer_name,
                to_peer_id=peer.peer_id,
                to_peer_name=peer.display_name,
                text=command.text,
                reply_to=command.reply_to,
                from_repowire_session_id=from_repowire_session_id,
                to_repowire_session_id=to_repowire_session_id,
                question=command.question,
            )
        except QuiescedError as e:
            raise AskServiceError(
                503,
                {
                    "error": "peer_switching",
                    "hint": f"Peer {command.to_peer} is mid-switch; retry shortly.",
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
            ask_outcome = await peer_delivery.deliver_ask(
                from_peer=from_peer_id,
                to_peer=peer.peer_id,
                text=command.text,
                correlation_id=cid,
                reply_to=command.reply_to,
                bypass_circle=True,
                attachments=command.attachments,
                on_acp_complete=self._make_on_acp_complete(),
                question=command.question,
            )
        except ValueError as e:
            if trace is not None:
                trace.record(
                    trace_id=cid, kind="ask", stage="resolve_failed", status="fail",
                    peer_id=peer.peer_id, detail={"error": str(e)},
                )
            await ask_tracker.close(cid, reason="evicted")
            raise AskServiceError(404, str(e)) from e
        except DeliveryInjectionError as e:
            if trace is not None:
                trace.record_outcome(
                    trace_id=cid,
                    kind="ask",
                    peer_id=peer.peer_id,
                    from_peer_id=from_peer_id,
                    transport="ws",
                    hook_delivery=e.hook_delivery,
                )
            await ask_tracker.close(cid, reason="send_failed")
            raise AskServiceError(
                503,
                {
                    "error": "injection_failed",
                    "hint": f"Ask injection failed for {command.to_peer}: {e}",
                    "correlation_id": cid,
                },
            ) from e
        except TransportError as e:
            if uses_cli_polling_fallback(peer):
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
                        text=command.text,
                        attachments=dump_attachments(command.attachments),
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
                if command.reply_to:
                    prior = await ask_tracker.close(command.reply_to, reason="reply_to")
                    if prior is None:
                        logger.debug(
                            "ask reply_to=%s: prior ask not found or already closed",
                            command.reply_to,
                        )
                return OpenAskResult(correlation_id=cid)
            if trace is not None:
                trace.record(
                    trace_id=cid, kind="ask", stage="no_connection", status="fail",
                    peer_id=peer.peer_id, detail={"error": str(e)},
                )
            await ask_tracker.close(cid, reason="send_failed")
            raise AskServiceError(
                503,
                f"Peer {command.to_peer} has no live connection: {e}",
            ) from e

        if trace is not None:
            trace.record_outcome(
                trace_id=cid,
                kind="ask",
                peer_id=peer.peer_id,
                from_peer_id=from_peer_id,
                transport=ask_outcome.transport,
                hook_delivery=ask_outcome.hook_delivery,
            )

        if command.reply_to:
            prior = await ask_tracker.close(command.reply_to, reason="reply_to")
            if prior is None:
                logger.debug(
                    "ask reply_to=%s: prior ask not found or already closed",
                    command.reply_to,
                )

        return OpenAskResult(correlation_id=cid)

    async def answer(self, command: AnswerCommand) -> None:
        """Answer a structured question ask."""
        peer_registry = self._peer_registry
        state = self._state
        ask_tracker = self._ask_tracker

        existing = await ask_tracker.get(command.correlation_id)
        if existing is None:
            raise AskServiceError(
                404, f"No open ask with correlation_id: {command.correlation_id}",
            )
        if existing.question is None:
            raise AskServiceError(
                422,
                f"Ask {command.correlation_id} is not a structured question; use /ack.",
            )
        answer = Answer(
            option_id=command.option_id,
            text=command.text,
            outcome=command.outcome,
            message=command.message,
        )
        try:
            await ask_tracker.answer(command.correlation_id, answer)
        except ask_tracker.AlreadyAnsweredError as e:
            raise AskServiceError(
                410, f"Ask {command.correlation_id} is already answered/closed.",
            ) from e
        except ValueError as e:
            raise AskServiceError(422, str(e)) from e

        is_tool_permission = (
            existing.question is not None and existing.question.scope == "tool_permission"
        )
        body = None if is_tool_permission else self._answer_reply_text(existing, answer)
        delivery_success = False
        if body and existing.reply_delivery == "pull":
            # Asker is blocked in wait_on_ack; the recorded answer is returned
            # by the resolved waiter — no notify-back needed.
            delivery_success = True
        elif body:
            framed = f"[ack #{command.correlation_id} from @{existing.to_peer_name}] {body}"
            try:
                peer_delivery = peer_delivery_from_state(
                    config=state.config, registry=peer_registry, state=state,
                )
                await peer_delivery.notify(
                    from_peer=existing.to_peer_id,
                    to_peer=existing.from_peer_id,
                    text=framed,
                    bypass_circle=True,
                    attachments=command.attachments,
                )
                delivery_success = True
            except TransportError as e:
                identity = await self._capture_asker_identity(existing.from_peer_id)
                stashed = await ask_tracker.set_pending_reply(
                    command.correlation_id,
                    framed,
                    identity=identity,
                    allow_answered_question=True,
                )
                logger.warning(
                    "answer for %s recorded; asker delivery deferred (%s; stashed=%s)",
                    command.correlation_id, e, stashed,
                )
            except ValueError as e:
                logger.warning(
                    "answer for %s recorded; asker delivery deferred (%s)",
                    command.correlation_id, e,
                )
        self._emit_ack_event(
            ask=existing,
            reason="answered",
            delivered=delivery_success,
            has_message=bool(body),
            has_attachments=bool(command.attachments),
        )

    async def ack(self, command: AckCommand) -> None:
        """Close an ask, optionally delivering a reply to the asker."""
        peer_registry = self._peer_registry
        state = self._state
        ask_tracker = self._ask_tracker
        trace = getattr(state, "delivery_trace_store", None)

        existing = await ask_tracker.get(command.correlation_id)
        if existing is None:
            raise AskServiceError(
                404,
                f"No open ask with correlation_id: {command.correlation_id}",
            )
        if existing.question is not None and not existing.closed:
            delegated = AnswerCommand(
                correlation_id=command.correlation_id,
                text=command.message,
                outcome="acknowledged" if not command.message else "answered",
                attachments=command.attachments,
            )
            await self.answer(delegated)
            return
        if existing.closed and (command.message or command.attachments):
            raise AskServiceError(
                410,
                (
                    f"Ask {command.correlation_id} is already closed; "
                    "reply message was not delivered. Send a new notify/ask instead."
                ),
            )
        if existing.closed:
            return

        if existing.reply_delivery == "pull" and (command.message or command.attachments):
            # The asker is blocked in wait_on_ack: retain the reply on the ask
            # and let the resolved waiter deliver it as the tool result, instead
            # of injecting into a pane nobody is reading.
            await ask_tracker.capture_reply(
                command.correlation_id,
                command.message or "",
                attachments=[a.model_dump() for a in command.attachments or []] or None,
            )
            await ask_tracker.close(command.correlation_id, reason="ack_with_msg")
            self._emit_ack_event(
                ask=existing,
                reason="ack_with_msg",
                delivered=True,
                has_message=True,
                has_attachments=bool(command.attachments),
            )
        elif command.message or command.attachments:
            framed = (
                f"[ack #{command.correlation_id} from @{existing.to_peer_name}] "
                f"{command.message or ''}"
            )
            try:
                peer_delivery = peer_delivery_from_state(
                    config=state.config,
                    registry=peer_registry,
                    state=state,
                )
                delivery = await peer_delivery.notify_result(
                    from_peer=existing.to_peer_id,
                    to_peer=existing.from_peer_id,
                    text=framed,
                    bypass_circle=True,
                    attachments=command.attachments,
                )
            except ValueError as e:
                logger.warning(
                    "ack reply for %s: asker missing (%s); closing without delivery",
                    command.correlation_id, e,
                )
                await ask_tracker.close(command.correlation_id, reason="ack_with_msg")
                self._emit_ack_event(
                    ask=existing,
                    reason="ack_with_msg",
                    delivered=False,
                    has_message=True,
                    has_attachments=bool(command.attachments),
                )
                return
            except TransportError as e:
                raise AskServiceError(
                    503,
                    (
                        f"Reply delivery failed for {existing.from_peer_name}: {e}. "
                        "Ask remains open; retry when the asker reconnects."
                    ),
                ) from e
            except Exception as e:
                logger.exception(
                    "ack reply delivery error for %s: %s", command.correlation_id, e,
                )
                raise AskServiceError(500, f"Reply delivery error: {e}") from e
            if not delivery.delivered:
                raise AskServiceError(
                    503,
                    (
                        f"Reply delivery failed for {existing.from_peer_name}: "
                        f"{delivery.reason}. Ask remains open; retry when the "
                        "asker reconnects."
                    ),
                )

            if command.message:
                await ask_tracker.capture_reply(command.correlation_id, command.message)
            await ask_tracker.close(command.correlation_id, reason="ack_with_msg")
        else:
            self._emit_ack_event(
                ask=existing,
                reason="ack",
                delivered=False,
                has_message=False,
                has_attachments=False,
            )
            await ask_tracker.close(command.correlation_id, reason="ack")

        if trace is not None:
            trace.record(
                trace_id=command.correlation_id, kind="ask", stage="acked",
                peer_id=existing.to_peer_id, from_peer_id=existing.from_peer_id,
            )
            trace.record(
                trace_id=command.correlation_id, kind="ask", stage="closed",
                peer_id=existing.to_peer_id, from_peer_id=existing.from_peer_id,
            )

    async def acp_complete(
        self,
        *,
        correlation_id: str,
        reply: str | None,
        error: str | None,
    ) -> None:
        """Deliver an ACP-routed ask result back to the asker."""
        ask = await self._ask_tracker.get(correlation_id)
        if ask is None or ask.closed:
            return
        is_error = error is not None
        if not is_error and reply is not None and ask.reply_delivery == "pull":
            # Asker is blocked in wait_on_ack: retain the reply, skip the notify.
            await self._ask_tracker.capture_reply(correlation_id, reply)
            await self._ask_tracker.close(correlation_id, reason="ack_with_msg")
            self._emit_ack_event(
                ask=ask,
                reason="ack_with_msg",
                delivered=True,
                has_message=True,
                has_attachments=False,
            )
            return
        if is_error:
            framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] ACP error: {error}"
        else:
            body = reply or ""
            framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] {body}"
        try:
            await self._peer_registry.notify(
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
                await self._ask_tracker.close(correlation_id, reason="send_failed")
                self._emit_ack_event(
                    ask=ask,
                    reason="send_failed",
                    delivered=False,
                    has_message=True,
                    has_attachments=False,
                )
                return
            identity = await self._capture_asker_identity(ask.from_peer_id)
            stashed = await self._ask_tracker.set_pending_reply(
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
            await self._ask_tracker.close(correlation_id, reason="ack_with_msg")
            self._emit_ack_event(
                ask=ask,
                reason="ack_with_msg",
                delivered=False,
                has_message=not is_error,
                has_attachments=False,
            )
            return
        if not is_error and reply is not None:
            await self._ask_tracker.capture_reply(correlation_id, reply)
        await self._ask_tracker.close(
            correlation_id, reason="ack_with_msg" if not is_error else "send_failed",
        )
        self._emit_ack_event(
            ask=ask,
            reason="ack_with_msg" if not is_error else "send_failed",
            delivered=True,
            has_message=not is_error,
            has_attachments=False,
        )

    def make_on_acp_complete(self):
        async def _on_acp_complete(
            correlation_id: str, reply: str | None, error: str | None,
        ) -> None:
            await self.acp_complete(
                correlation_id=correlation_id,
                reply=reply,
                error=error,
            )

        return _on_acp_complete

    def _make_on_acp_complete(self):
        return self.make_on_acp_complete()

    async def _get_peer_or_error(
        self, identifier: str, *, circle: str | None = None,
    ) -> Peer:
        try:
            peer = await self._peer_registry.get_peer(identifier, circle=circle)
        except ValueError as e:
            raise AskServiceError(409, str(e)) from e
        if not peer:
            raise AskServiceError(404, f"Unknown peer: {identifier}")
        return peer

    async def _resolve_sender_for_target(
        self, from_peer: str, target: Peer,
    ) -> Peer | None:
        try:
            return (
                await self._peer_registry.get_peer(from_peer, circle=target.circle)
                or await self._peer_registry.get_peer(from_peer)
            )
        except ValueError as e:
            raise AskServiceError(409, str(e)) from e

    async def _capture_asker_identity(self, asker_peer_id: str) -> AskerIdentity | None:
        try:
            peer = await self._peer_registry.get_peer(asker_peer_id)
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

    def _binding_id_for_peer(self, peer: Peer | None) -> str | None:
        return resolve_repowire_session_id(
            getattr(self._state, "session_binding_store", None),
            peer=peer,
        )

    def _emit_ack_event(
        self,
        *,
        ask: Ask,
        reason: str,
        delivered: bool,
        has_message: bool,
        has_attachments: bool,
    ) -> None:
        self._peer_registry.add_event(
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

    @staticmethod
    def _answer_reply_text(ask: Ask, answer: Answer) -> str | None:
        if answer.text:
            return answer.text
        if answer.option_id and ask.question is not None:
            for opt in ask.question.options:
                if opt.id == answer.option_id:
                    return opt.title
            return answer.option_id
        return answer.message
