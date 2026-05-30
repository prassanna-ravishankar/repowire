"""Message routing logic.

Routes messages via WebSocket transport.
"""

import asyncio
import logging
from typing import Any
from uuid import uuid4

from repowire.config.models import DEFAULT_QUERY_TIMEOUT
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.websocket_transport import TransportError, WebSocketTransport
from repowire.protocol.messages import AttachmentRef

logger = logging.getLogger(__name__)


def _dump_attachments(
    attachments: list[AttachmentRef] | list[dict[str, Any]] | None,
) -> list[dict[str, Any]]:
    if not attachments:
        return []
    return [
        a.model_dump(exclude_none=True) if isinstance(a, AttachmentRef) else a
        for a in attachments
    ]


class MessageRouter:
    """Routes messages via WebSocket."""

    def __init__(
        self,
        transport: WebSocketTransport,
        query_tracker: QueryTracker,
    ):
        self._transport = transport
        self._query_tracker = query_tracker

    async def send_query(
        self,
        from_peer: str,
        to_session_id: str,
        to_peer_name: str,
        text: str,
        timeout: float = DEFAULT_QUERY_TIMEOUT,
    ) -> str:
        """Send query and wait for response.

        Args:
            from_peer: Display name of sender
            to_session_id: Session ID of recipient
            to_peer_name: Display name of recipient (for logging)
            text: Query text
            timeout: Timeout in seconds

        Returns:
            Response text

        Raises:
            ValueError: If peer not connected
            TimeoutError: If no response within timeout
            TransportError: If send fails
        """
        if not self._transport.is_connected(to_session_id):
            raise ValueError(f"Peer {to_peer_name} not connected")

        # Register query
        correlation_id = await self._query_tracker.register_query(
            from_peer=from_peer,
            to_peer_id=to_session_id,
            to_peer_name=to_peer_name,
            query_text=text,
        )

        future = self._query_tracker.get_future(correlation_id)
        if not future:
            raise ValueError("Query tracking error")

        # Send via WebSocket
        message: dict[str, Any] = {
            "type": "query",
            "correlation_id": correlation_id,
            "from_peer": from_peer,
            "text": text,
        }

        try:
            await self._transport.send(to_session_id, message)
            logger.info(f"Query sent: {from_peer} -> {to_peer_name} ({correlation_id[:8]})")

            # Wait for response
            response = await asyncio.wait_for(future, timeout=timeout)
            logger.info(f"Query resolved: {from_peer} -> {to_peer_name} ({correlation_id[:8]})")
            return response

        except asyncio.TimeoutError:
            logger.warning(f"Query timeout: {from_peer} -> {to_peer_name} ({correlation_id[:8]})")
            raise TimeoutError(f"No response from {to_peer_name} within {timeout}s")

        except TransportError as e:
            logger.error(f"Transport error: {e}")
            raise

        finally:
            await self._query_tracker.cleanup_query(correlation_id)

    async def send_notification(
        self,
        from_peer: str,
        to_session_id: str,
        to_peer_name: str,
        text: str,
        intended_recipient_name: str | None = None,
        attachments: list[AttachmentRef] | list[dict[str, Any]] | None = None,
        delivery_id: str | None = None,
    ) -> dict[str, Any] | None:
        """Send a plain FYI notification (fire-and-forget, no lifecycle).

        Wire shape: {type: notify, delivery_id, from_peer, text}. Newer hooks
        may return an optional delivery_ack frame describing terminal injection
        (injected/rejected/failed). Older hooks ignore delivery_id, so a missing
        ack is not an error.

        Raises:
            TransportError: If send fails
        """
        message: dict[str, Any] = {
            "type": "notify",
            "delivery_id": delivery_id or f"notif-delivery-{uuid4().hex[:8]}",
            "from_peer": from_peer,
            "to_peer": to_peer_name,
            "text": text,
        }
        if attachments:
            message["attachments"] = _dump_attachments(attachments)
        logger.info(
            "Notify delivery trace: sender_identity=%s intended_recipient_name=%s "
            "resolved_peer_id=%s frame.to_peer=%s actual_delivered_pane_id=%s",
            from_peer,
            intended_recipient_name or to_peer_name,
            to_session_id,
            message["to_peer"],
            self._transport.get_connection_pane_id(to_session_id),
        )
        delivery_ack = await self._transport.send_and_wait_delivery_ack(
            to_session_id,
            message,
        )
        if delivery_ack is None:
            logger.info(f"Notification sent: {from_peer} -> {to_peer_name}")
        else:
            logger.info(
                "Notification sent: %s -> %s (hook_delivery=%s)",
                from_peer,
                to_peer_name,
                delivery_ack.get("status"),
            )
        return delivery_ack

    async def send_ask(
        self,
        from_peer: str,
        to_session_id: str,
        to_peer_name: str,
        correlation_id: str,
        text: str,
        reply_to: str | None = None,
        intended_recipient_name: str | None = None,
        attachments: list[AttachmentRef] | list[dict[str, Any]] | None = None,
    ) -> dict[str, Any] | None:
        """Send a first-class ask wire message.

        Wire shape: {type: ask, correlation_id, from_peer, text, reply_to?}.
        The receiving transport dispatches type=ask explicitly and surfaces
        the message to the agent (e.g. tmux paste). The daemon doesn't track
        pickup state — open asks reappear in every Stop hook reminder until
        acked.

        Raises:
            TransportError: If send fails
        """
        hinted_text = (
            f'{text.rstrip()}\n'
            f'↳ ack("{correlation_id}") or ack("{correlation_id}", "reply")'
        )
        message: dict[str, Any] = {
            "type": "ask",
            "delivery_id": f"ask-delivery-{uuid4().hex[:8]}",
            "correlation_id": correlation_id,
            "from_peer": from_peer,
            "to_peer": to_peer_name,
            "text": hinted_text,
        }
        if attachments:
            message["attachments"] = _dump_attachments(attachments)
        if reply_to is not None:
            message["reply_to"] = reply_to
        logger.info(
            "Ask delivery trace: sender_identity=%s intended_recipient_name=%s "
            "resolved_peer_id=%s frame.to_peer=%s actual_delivered_pane_id=%s",
            from_peer,
            intended_recipient_name or to_peer_name,
            to_session_id,
            message["to_peer"],
            self._transport.get_connection_pane_id(to_session_id),
        )
        delivery_ack = await self._transport.send_and_wait_delivery_ack(
            to_session_id,
            message,
        )
        if isinstance(delivery_ack, dict) and delivery_ack.get("status") in {
            "failed",
            "rejected",
        }:
            detail = delivery_ack.get("detail") or delivery_ack.get("status")
            raise TransportError(f"Ask injection {delivery_ack.get('status')}: {detail}")
        logger.info(f"Ask sent: {from_peer} -> {to_peer_name} ({correlation_id[:8]})")
        return delivery_ack if isinstance(delivery_ack, dict) else None

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: set[str] | None = None,
    ) -> tuple[list[str], list[dict[str, str]]]:
        """Best-effort broadcast to all connected peers (minus excludes).

        Returns:
            (sent_session_ids, failed) where failed is [{session_id, error}, ...]
            for recipients whose transport raised. One failure does not abort
            the rest of the fanout.
        """
        excluded = exclude or set()
        message: dict[str, Any] = {
            "type": "broadcast",
            "from_peer": from_peer,
            "text": text,
        }

        async def _send_one(session_id: str) -> tuple[str, str | None]:
            try:
                await self._transport.send(session_id, message)
                return session_id, None
            except TransportError as e:
                logger.warning(f"Broadcast to {session_id} failed: {e}")
                return session_id, str(e)

        targets = [sid for sid in self._transport.get_all_sessions() if sid not in excluded]
        results = await asyncio.gather(*(_send_one(sid) for sid in targets))
        sent_to = [sid for sid, err in results if err is None]
        failed = [{"session_id": sid, "error": err} for sid, err in results if err is not None]

        logger.info(
            "Broadcast from %s: sent to %d peers, %d failed",
            from_peer, len(sent_to), len(failed),
        )
        return sent_to, failed
