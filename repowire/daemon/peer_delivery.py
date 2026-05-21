"""Peer delivery application service.

This module owns delivery orchestration that used to live directly on
``PeerRegistry``. The registry remains the source of peer identity, live state,
and circle access rules; this service composes that with the router layer.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Literal, cast
from uuid import uuid4

from repowire.config.models import DEFAULT_QUERY_TIMEOUT, Config
from repowire.daemon.ask_tracker import AskerIdentity, AskTracker
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry, normalize_identity_path
from repowire.daemon.transport_router import (
    AskCompletion,
    AskEnvelope,
    NotifyEnvelope,
    PeerTransportRouter,
    transport_router_from_state,
)
from repowire.protocol.messages import AttachmentRef

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class NotifyDeliveryResult:
    """Structured notify outcome for HTTP clients.

    ``status`` is the legacy compact value returned by older call sites.
    ``delivery_state`` and ``reason`` make the same outcome explicit so
    clients do not need to infer BUSY queueing or transport success.
    """

    status: Literal["sent", "queued"]
    delivery_state: Literal["delivered", "queued"]
    reason: Literal["transport_delivered", "recipient_busy"]
    from_peer_id: str | None
    from_peer_name: str
    to_peer_id: str
    to_peer_name: str

    @property
    def delivered(self) -> bool:
        return self.delivery_state == "delivered"

    @property
    def queued(self) -> bool:
        return self.delivery_state == "queued"


class PeerDeliveryService:
    """Coordinates peer-to-peer delivery across legacy and transport-routed paths."""

    def __init__(
        self,
        *,
        config: Config,
        registry: PeerRegistry,
        message_router: MessageRouter,
        transport_router: PeerTransportRouter | None = None,
        ask_tracker: AskTracker | None = None,
    ) -> None:
        self._config = config
        self._registry = registry
        self._message_router = message_router
        self._transport_router = transport_router
        self._ask_tracker = ask_tracker

    async def query(
        self,
        *,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = DEFAULT_QUERY_TIMEOUT,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> str:
        """Send a legacy query to a peer and wait for its stop-hook response."""
        from_obj, peer = await self._registry.check_access(
            from_peer=from_peer,
            to_peer=to_peer,
            bypass_circle=bypass_circle,
            circle=circle,
        )
        from_peer_id = from_obj.peer_id if from_obj else None

        formatted_query = (
            f"[Repowire Query from @{from_peer}]\n"
            f"{text}\n\n"
            f"IMPORTANT: Respond directly in your message. Do NOT use ask() to reply - "
            f"your response is automatically captured and returned to {from_peer}."
        )

        query_event_id = self._registry.add_event(
            "query",
            {
                "from": from_peer,
                "to": to_peer,
                "text": text,
                "from_peer_id": from_peer_id,
                "to_peer_id": peer.peer_id,
                "status": "pending",
            },
        )

        try:
            response = await self._message_router.send_query(
                from_peer=from_peer,
                to_session_id=peer.peer_id,
                to_peer_name=peer.display_name,
                text=formatted_query,
                timeout=timeout,
            )
            self._registry._update_event(query_event_id, {"status": "success"})
            self._registry.add_event(
                "response",
                {
                    "from": to_peer,
                    "to": from_peer,
                    "from_peer_id": peer.peer_id,
                    "to_peer_id": from_peer_id,
                    "text": response[:100] + "..." if len(response) > 100 else response,
                    "correlation_id": query_event_id,
                },
            )
            return response
        except TimeoutError:
            self._registry._update_event(query_event_id, {"status": "timeout"})
            asyncio.ensure_future(self._registry._check_peer_after_timeout(peer.peer_id))
            raise
        except Exception as e:
            self._registry._update_event(
                query_event_id, {"status": "error", "error": str(e)}
            )
            raise

    async def notify(
        self,
        *,
        from_peer: str,
        to_peer: str,
        text: str,
        bypass_circle: bool = False,
        circle: str | None = None,
        attachments: list[AttachmentRef] | tuple[AttachmentRef, ...] | None = None,
    ) -> Literal["sent", "queued"]:
        """Send a fire-and-forget notification using ACP-before-WS routing."""
        result = await self.notify_result(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            bypass_circle=bypass_circle,
            circle=circle,
            attachments=attachments,
        )
        return result.status

    async def notify_result(
        self,
        *,
        from_peer: str,
        to_peer: str,
        text: str,
        bypass_circle: bool = False,
        circle: str | None = None,
        attachments: list[AttachmentRef] | tuple[AttachmentRef, ...] | None = None,
    ) -> NotifyDeliveryResult:
        """Send a notification and return an explicit delivery outcome."""
        from_obj, target = await self._registry.check_access(
            from_peer=from_peer,
            to_peer=to_peer,
            bypass_circle=bypass_circle,
            circle=circle,
        )
        attachment_tuple = tuple(attachments or ())
        delivery_status = await self._router().send_notify(
            NotifyEnvelope(
                from_peer_id=from_obj.peer_id if from_obj else None,
                from_peer_name=from_obj.display_name if from_obj else from_peer,
                target=target,
                text=text,
                intended_recipient_name=to_peer,
                attachments=attachment_tuple,
            )
        )
        delivery_state: Literal["delivered", "queued"] = (
            "queued" if delivery_status == "queued" else "delivered"
        )
        reason: Literal["transport_delivered", "recipient_busy"] = (
            "recipient_busy" if delivery_status == "queued" else "transport_delivered"
        )
        return NotifyDeliveryResult(
            status=delivery_status,
            delivery_state=delivery_state,
            reason=reason,
            from_peer_id=from_obj.peer_id if from_obj else None,
            from_peer_name=from_obj.display_name if from_obj else from_peer,
            to_peer_id=target.peer_id,
            to_peer_name=target.display_name,
        )

    async def deliver_ask(
        self,
        *,
        from_peer: str,
        to_peer: str,
        text: str,
        correlation_id: str,
        reply_to: str | None = None,
        bypass_circle: bool = False,
        circle: str | None = None,
        attachments: list[AttachmentRef] | tuple[AttachmentRef, ...] | None = None,
        on_acp_complete: AskCompletion | None = None,
    ) -> None:
        """Deliver an already-registered ask using ACP-before-WS routing."""
        from_obj, target = await self._registry.check_access(
            from_peer=from_peer,
            to_peer=to_peer,
            bypass_circle=bypass_circle,
            circle=circle,
        )
        from_peer_id = from_obj.peer_id if from_obj else from_peer
        from_peer_name = from_obj.display_name if from_obj else from_peer
        completion = on_acp_complete or self._default_acp_completion()

        await self._router().send_ask(
            AskEnvelope(
                from_peer_id=from_peer_id,
                from_peer_name=from_peer_name,
                target=target,
                text=text,
                correlation_id=correlation_id,
                reply_to=reply_to,
                intended_recipient_name=to_peer,
                attachments=tuple(attachments or ()),
            ),
            on_acp_complete=completion,
        )

    async def open_scheduled_ask(
        self,
        *,
        from_peer: str,
        to_peer: str,
        text: str,
        circle: str | None = None,
    ) -> str:
        """Register and deliver a scheduled ask, rolling back on send failure."""
        if self._ask_tracker is None:
            raise RuntimeError("AskTracker is required to open scheduled asks")
        cid = f"ask-{uuid4().hex[:8]}"
        await self._ask_tracker.register(
            from_peer_id=from_peer,
            from_peer_name=from_peer,
            to_peer_id=to_peer,
            to_peer_name=to_peer,
            text=text,
            correlation_id=cid,
        )
        try:
            await self.deliver_ask(
                from_peer=from_peer,
                to_peer=to_peer,
                text=text,
                correlation_id=cid,
                circle=circle,
            )
        except Exception:
            await self._ask_tracker.close(cid, reason="send_failed")
            raise
        return cid

    async def broadcast(
        self,
        *,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> tuple[list[str], list[dict[str, str]]]:
        """Broadcast to all eligible connected peers, best-effort per recipient."""
        exclude_names = set(exclude or [])
        exclude_names.add(from_peer)

        exclude_session_ids: set[str] = set()
        from_peer_obj = await self._registry.get_peer(from_peer)
        for name in exclude_names:
            peer = await self._registry.get_peer(name)
            if peer:
                exclude_session_ids.add(peer.peer_id)

        sender_bypasses = (
            from_peer_obj is not None
            and (bypass_circle or from_peer_obj.bypasses_circles)
        )
        peers = await self._registry.get_all_peers()
        if from_peer_obj is not None and not sender_bypasses:
            for peer in peers:
                if peer.circle != from_peer_obj.circle and not peer.bypasses_circles:
                    exclude_session_ids.add(peer.peer_id)

        id_to_name = {peer.peer_id: peer.display_name for peer in peers}
        self._registry.add_event(
            "broadcast",
            {
                "from": from_peer,
                "text": text,
                "exclude": exclude,
                "from_peer_id": from_peer_obj.peer_id if from_peer_obj else None,
            },
        )

        sent_ids, failed = await self._message_router.broadcast(
            from_peer=from_peer,
            text=text,
            exclude=exclude_session_ids,
        )
        return (
            [id_to_name[sid] for sid in sent_ids if sid in id_to_name],
            [
                {
                    "peer": id_to_name.get(f["session_id"], f["session_id"]),
                    "error": f["error"],
                }
                for f in failed
            ],
        )

    def _router(self) -> PeerTransportRouter:
        if self._transport_router is None:
            raise RuntimeError("PeerTransportRouter not configured")
        return self._transport_router

    def _default_acp_completion(self) -> AskCompletion:
        if self._ask_tracker is None:
            async def _noop(
                _correlation_id: str,
                _reply: str | None,
                _error: str | None,
            ) -> None:
                return None

            return cast(AskCompletion, _noop)

        async def _complete(
            correlation_id: str,
            reply: str | None,
            error: str | None,
        ) -> None:
            await self._complete_acp_ask(
                correlation_id=correlation_id,
                reply=reply,
                error=error,
            )

        return cast(AskCompletion, _complete)

    async def _complete_acp_ask(
        self,
        *,
        correlation_id: str,
        reply: str | None,
        error: str | None,
    ) -> None:
        assert self._ask_tracker is not None
        ask = await self._ask_tracker.get(correlation_id)
        if ask is None or ask.closed:
            return
        is_error = error is not None
        if is_error:
            framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] ACP error: {error}"
        else:
            framed = f"[ack #{correlation_id} from @{ask.to_peer_name}] {reply or ''}"
        try:
            await self.notify(
                from_peer=ask.to_peer_id,
                to_peer=ask.from_peer_id,
                text=framed,
                bypass_circle=True,
            )
        except Exception as e:
            from repowire.daemon.websocket_transport import TransportError

            if isinstance(e, TransportError) and not is_error:
                identity = await self._capture_asker_identity(ask.from_peer_id)
                stashed = await self._ask_tracker.set_pending_reply(
                    correlation_id, framed, identity=identity,
                )
                logger.warning(
                    "ACP ack reply for cid=%s: asker offline (%s). %s%s; "
                    "will redeliver on reconnect.",
                    correlation_id,
                    e,
                    "Reply stashed" if stashed else "Stash failed (ask closed)",
                    " with identity tuple" if (stashed and identity) else "",
                )
                return
            if isinstance(e, TransportError):
                logger.warning(
                    "ACP ack error-frame for cid=%s undeliverable (%s); closing.",
                    correlation_id,
                    e,
                )
                await self._ask_tracker.close(correlation_id, reason="send_failed")
                return
            logger.warning(
                "ACP ack reply for cid=%s: asker missing (%s). Closing.",
                correlation_id,
                e,
            )
            await self._ask_tracker.close(correlation_id, reason="ack_with_msg")
            return
        await self._ask_tracker.close(
            correlation_id,
            reason="ack_with_msg" if not is_error else "send_failed",
        )

    async def _capture_asker_identity(self, asker_peer_id: str) -> AskerIdentity | None:
        try:
            peer = await self._registry.get_peer(asker_peer_id)
        except ValueError:
            return None
        if peer is None:
            return None
        if not (peer.display_name and peer.circle and peer.backend):
            return None
        if not peer.machine or peer.machine == "unknown":
            return None
        norm_path = normalize_identity_path(peer.path or "")
        if not norm_path:
            return None
        return AskerIdentity(
            display_name=peer.display_name,
            circle=peer.circle,
            backend=peer.backend.value,
            path=norm_path,
            machine=peer.machine,
        )


def peer_delivery_from_state(
    *,
    config: Config,
    registry: PeerRegistry,
    state: object,
) -> PeerDeliveryService:
    """Return the app-state delivery service, building a fallback for tests."""
    existing = getattr(state, "peer_delivery", None)
    if existing is not None:
        return existing
    transport_router = transport_router_from_state(
        config=config,
        registry=registry,
        state=state,
    )
    return PeerDeliveryService(
        config=config,
        registry=registry,
        message_router=getattr(state, "message_router"),
        transport_router=transport_router,
        ask_tracker=getattr(state, "ask_tracker", None),
    )
