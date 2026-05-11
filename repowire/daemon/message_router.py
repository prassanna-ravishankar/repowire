"""Message routing logic.

Routes messages via WebSocket transport.
"""

import asyncio
import logging
from typing import Any

from repowire.config.models import DEFAULT_QUERY_TIMEOUT
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.websocket_transport import TransportError, WebSocketTransport

logger = logging.getLogger(__name__)


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
    ) -> None:
        """Send a plain FYI notification (fire-and-forget, no lifecycle).

        Wire shape: {type: notify, from_peer, text}. Use send_ask for
        ask-lifecycle messages.

        Raises:
            TransportError: If send fails
        """
        message: dict[str, Any] = {
            "type": "notify",
            "from_peer": from_peer,
            "text": text,
        }
        await self._transport.send(to_session_id, message)
        logger.info(f"Notification sent: {from_peer} -> {to_peer_name}")

    async def send_ask(
        self,
        from_peer: str,
        to_session_id: str,
        to_peer_name: str,
        correlation_id: str,
        text: str,
        reply_to: str | None = None,
    ) -> None:
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
            "correlation_id": correlation_id,
            "from_peer": from_peer,
            "text": hinted_text,
        }
        if reply_to is not None:
            message["reply_to"] = reply_to
        await self._transport.send(to_session_id, message)
        logger.info(f"Ask sent: {from_peer} -> {to_peer_name} ({correlation_id[:8]})")

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
