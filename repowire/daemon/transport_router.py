"""Transport selection for peer-to-peer delivery.

Routes resolve identity and authorization, then hand an envelope here. This
module owns the transport choice (currently ACP-before-WebSocket) without
moving runtime/session ownership into the HTTP layer.
"""

from __future__ import annotations

import uuid
from collections.abc import Awaitable
from dataclasses import dataclass
from typing import Any, Literal, Protocol

from repowire.acp import AcpRouteDecision, deliver_ask_via_acp, maybe_decide_acp_route
from repowire.config.models import Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.messages import AttachmentRef
from repowire.protocol.peers import Peer, PeerStatus


def _text_with_attachment_fallback(text: str, attachments: tuple[AttachmentRef, ...]) -> str:
    if not attachments:
        return text
    lines = ["", "Attachments:"]
    for attachment in attachments:
        label = attachment.filename or attachment.path or attachment.id or "attachment"
        target = attachment.path or (f"/attachments/{attachment.id}" if attachment.id else "")
        lines.append(f"- {label}: {target}" if target else f"- {label}")
    return text + "\n".join(lines)


@dataclass(frozen=True)
class AskEnvelope:
    """Already-authorized ask delivery request."""

    from_peer_id: str
    from_peer_name: str
    target: Peer
    text: str
    correlation_id: str
    reply_to: str | None = None
    intended_recipient_name: str | None = None
    attachments: tuple[AttachmentRef, ...] = ()
    from_repowire_session_id: str | None = None
    to_repowire_session_id: str | None = None


@dataclass(frozen=True)
class NotifyEnvelope:
    """Already-authorized notification delivery request."""

    from_peer_id: str | None
    from_peer_name: str
    target: Peer
    text: str
    intended_recipient_name: str | None = None
    attachments: tuple[AttachmentRef, ...] = ()
    from_repowire_session_id: str | None = None
    to_repowire_session_id: str | None = None


@dataclass(frozen=True)
class NotifyTransportResult:
    """Transport handoff plus optional hook-level delivery receipt."""

    status: Literal["sent", "queued"]
    hook_delivery: dict[str, Any] | None = None
    repowire_session_id: str | None = None
    from_repowire_session_id: str | None = None
    to_repowire_session_id: str | None = None


class AskCompletion(Protocol):
    def __call__(
        self,
        correlation_id: str,
        reply: str | None,
        error: str | None,
    ) -> Awaitable[None]: ...


class WebSocketPeerTransport:
    """WebSocket delivery using the existing message-router wire format."""

    def __init__(self, registry: PeerRegistry, router: MessageRouter) -> None:
        self._registry = registry
        self._router = router

    async def send_ask(self, envelope: AskEnvelope) -> None:
        self._registry.add_event(
            "ask",
            {
                "from": envelope.from_peer_name,
                "to": envelope.target.display_name,
                "text": envelope.text,
                "from_peer_id": envelope.from_peer_id,
                "to_peer_id": envelope.target.peer_id,
                "correlation_id": envelope.correlation_id,
                "reply_to": envelope.reply_to,
                "self_target": envelope.from_peer_id == envelope.target.peer_id,
                "attachments": [a.model_dump(exclude_none=True) for a in envelope.attachments],
                "repowire_session_id": envelope.to_repowire_session_id,
                "from_repowire_session_id": envelope.from_repowire_session_id,
                "to_repowire_session_id": envelope.to_repowire_session_id,
            },
        )
        await self._router.send_ask(
            from_peer=envelope.from_peer_name,
            to_session_id=envelope.target.peer_id,
            to_peer_name=envelope.target.display_name,
            correlation_id=envelope.correlation_id,
            text=envelope.text,
            reply_to=envelope.reply_to,
            intended_recipient_name=(
                envelope.intended_recipient_name or envelope.target.display_name
            ),
            attachments=list(envelope.attachments),
        )

    async def send_notify(
        self,
        envelope: NotifyEnvelope,
    ) -> NotifyTransportResult:
        delivery_status: Literal["sent", "queued"] = (
            "queued" if envelope.target.status == PeerStatus.BUSY else "sent"
        )
        self._registry.add_event(
            "notification",
            {
                "from": envelope.from_peer_name,
                "to": envelope.target.display_name,
                "text": envelope.text,
                "from_peer_id": envelope.from_peer_id,
                "to_peer_id": envelope.target.peer_id,
                "delivery_status": delivery_status,
                "attachments": [a.model_dump(exclude_none=True) for a in envelope.attachments],
                "repowire_session_id": envelope.to_repowire_session_id,
                "from_repowire_session_id": envelope.from_repowire_session_id,
                "to_repowire_session_id": envelope.to_repowire_session_id,
            },
        )
        hook_delivery = await self._router.send_notification(
            from_peer=envelope.from_peer_name,
            to_session_id=envelope.target.peer_id,
            to_peer_name=envelope.target.display_name,
            text=envelope.text,
            intended_recipient_name=(
                envelope.intended_recipient_name or envelope.target.display_name
            ),
            attachments=list(envelope.attachments),
        )
        if not isinstance(hook_delivery, dict):
            hook_delivery = None
        return NotifyTransportResult(
            status=delivery_status,
            hook_delivery=hook_delivery,
            repowire_session_id=envelope.to_repowire_session_id,
            from_repowire_session_id=envelope.from_repowire_session_id,
            to_repowire_session_id=envelope.to_repowire_session_id,
        )


class AcpPeerTransport:
    """ACP delivery mapped onto prompt calls."""

    def __init__(self, manager: Any) -> None:
        self._manager = manager

    def decide(self, target: Peer, *, flag_enabled: bool) -> AcpRouteDecision | None:
        return maybe_decide_acp_route(
            target,
            flag_enabled=flag_enabled,
            manager=self._manager,
        )

    async def send_ask(
        self,
        envelope: AskEnvelope,
        on_complete: AskCompletion,
        decision: AcpRouteDecision,
    ) -> None:
        if decision is None or decision.spec is None:
            raise RuntimeError("ACP transport selected without an ACP route")
        await deliver_ask_via_acp(
            manager=self._manager,
            spec=decision.spec,
            correlation_id=envelope.correlation_id,
            text=_text_with_attachment_fallback(envelope.text, envelope.attachments),
            on_complete=on_complete,
        )

    async def send_notify(
        self,
        envelope: NotifyEnvelope,
        decision: AcpRouteDecision,
    ) -> NotifyTransportResult:
        if decision is None or decision.spec is None:
            raise RuntimeError("ACP transport selected without an ACP route")

        async def _drop_result(
            _cid: str,
            _reply: str | None,
            _err: str | None,
        ) -> None:
            # Notify is fire-and-forget: ACP prompt replies are intentionally
            # discarded after deliver_ask_via_acp logs failures.
            return None

        cid = f"notif-{uuid.uuid4().hex[:8]}"
        await deliver_ask_via_acp(
            manager=self._manager,
            spec=decision.spec,
            correlation_id=cid,
            text=_text_with_attachment_fallback(envelope.text, envelope.attachments),
            on_complete=_drop_result,
        )
        return NotifyTransportResult(
            status="sent",
            repowire_session_id=envelope.to_repowire_session_id,
            from_repowire_session_id=envelope.from_repowire_session_id,
            to_repowire_session_id=envelope.to_repowire_session_id,
        )


class PeerTransportRouter:
    """Choose ACP before WebSocket for peer delivery."""

    def __init__(
        self,
        *,
        config: Config,
        registry: PeerRegistry,
        message_router: MessageRouter,
        acp_manager: Any | None = None,
    ) -> None:
        self._config = config
        self._registry = registry
        self._ws = WebSocketPeerTransport(registry, message_router)
        self._acp = AcpPeerTransport(acp_manager) if acp_manager is not None else None

    def _acp_decision(self, target: Peer) -> AcpRouteDecision | None:
        experiments = self._config.experiments
        flag = bool(experiments.acp_broker_client) if experiments else False
        if not flag or self._acp is None:
            return None
        return self._acp.decide(target, flag_enabled=flag)

    async def send_ask(
        self,
        envelope: AskEnvelope,
        *,
        on_acp_complete: AskCompletion,
    ) -> None:
        decision = self._acp_decision(envelope.target)
        if decision is not None:
            assert self._acp is not None
            self._registry.add_event(
                "ask",
                {
                    "from": envelope.from_peer_name,
                    "to": envelope.target.display_name,
                    "text": envelope.text,
                    "from_peer_id": envelope.from_peer_id,
                    "to_peer_id": envelope.target.peer_id,
                    "correlation_id": envelope.correlation_id,
                    "reply_to": envelope.reply_to,
                    "self_target": envelope.from_peer_id == envelope.target.peer_id,
                    "attachments": [
                        a.model_dump(exclude_none=True) for a in envelope.attachments
                    ],
                    "repowire_session_id": envelope.to_repowire_session_id,
                    "from_repowire_session_id": envelope.from_repowire_session_id,
                    "to_repowire_session_id": envelope.to_repowire_session_id,
                },
            )
            await self._acp.send_ask(envelope, on_acp_complete, decision)
            return
        await self._ws.send_ask(envelope)

    async def send_notify(self, envelope: NotifyEnvelope) -> NotifyTransportResult:
        decision = self._acp_decision(envelope.target)
        if decision is not None:
            assert self._acp is not None
            self._registry.add_event(
                "notification",
                {
                    "from": envelope.from_peer_name,
                    "to": envelope.target.display_name,
                    "text": envelope.text,
                    "from_peer_id": envelope.from_peer_id,
                    "to_peer_id": envelope.target.peer_id,
                    "delivery_status": "sent",
                    "attachments": [
                        a.model_dump(exclude_none=True) for a in envelope.attachments
                    ],
                    "repowire_session_id": envelope.to_repowire_session_id,
                    "from_repowire_session_id": envelope.from_repowire_session_id,
                    "to_repowire_session_id": envelope.to_repowire_session_id,
                },
            )
            return await self._acp.send_notify(envelope, decision)
        return await self._ws.send_notify(envelope)


def transport_router_from_state(
    *,
    config: Config,
    registry: PeerRegistry,
    state: object,
) -> PeerTransportRouter:
    """Build the request-scoped router from daemon app state."""
    message_router = getattr(state, "message_router")
    acp_manager = getattr(state, "acp_manager", None)
    return PeerTransportRouter(
        config=config,
        registry=registry,
        message_router=message_router,
        acp_manager=acp_manager,
    )
