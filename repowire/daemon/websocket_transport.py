"""WebSocket transport layer.

Handles WebSocket connections, status tracking, and message delivery.
"""

import asyncio
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from fastapi import WebSocket

logger = logging.getLogger(__name__)


class TransportError(Exception):
    """Transport error."""


class DeliveryInjectionError(TransportError):
    """Wire send reached the hook but injection failed/was rejected at the pane.

    Carries the hook's terminal ``delivery_ack`` so callers can record a
    truthful injection_failed trace stage before surfacing the failure. This
    is distinct from a plain TransportError (no live connection), which should
    trace as no_connection.
    """

    def __init__(self, message: str, hook_delivery: dict | None = None) -> None:
        super().__init__(message)
        self.hook_delivery = hook_delivery


@dataclass
class ConnectionInfo:
    """Connection metadata."""

    session_id: str
    websocket: WebSocket
    pane_id: str | None = None
    display_name: str | None = None
    connected_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class WebSocketTransport:
    """Transport using WebSocket connections.

    Manages both the raw WebSocket connections and their associated metadata
    (status, connected_at).
    """

    def __init__(self) -> None:
        self._connections: dict[str, ConnectionInfo] = {}
        self._lock = asyncio.Lock()
        self._pong_futures: dict[str, asyncio.Future[dict]] = {}
        self._delivery_ack_futures: dict[tuple[str, str], asyncio.Future[dict]] = {}

    async def connect(
        self,
        session_id: str,
        websocket: WebSocket,
        pane_id: str | None = None,
        display_name: str | None = None,
    ) -> None:
        """Register WebSocket connection.

        If a connection already exists for this session_id, replace it
        (the old handler's finally block will clean itself up).
        """
        async with self._lock:
            if session_id in self._connections:
                logger.info(f"Replacing old connection for {session_id}")

            self._connections[session_id] = ConnectionInfo(
                session_id=session_id,
                websocket=websocket,
                pane_id=pane_id,
                display_name=display_name,
            )
            logger.info(f"Registered connection for {session_id}")

    async def disconnect(self, session_id: str, websocket: WebSocket | None = None) -> bool:
        """Unregister WebSocket connection.

        Args:
            session_id: Session to disconnect
            websocket: If provided, only disconnect if this is still the registered websocket.
                       Prevents race where an old handler disconnects a newer connection.

        Returns:
            True if the connection was actually removed, False if skipped (already replaced).
        """
        async with self._lock:
            if session_id in self._connections:
                if (
                    websocket is not None
                    and self._connections[session_id].websocket is not websocket
                ):
                    logger.debug(
                        f"Skipping disconnect for {session_id}: websocket already replaced"
                    )
                    return False
                self._connections.pop(session_id)
                logger.info(f"Unregistered connection for {session_id}")
                return True
            return False

    async def send(self, session_id: str, message: dict[str, Any]) -> None:
        """Send JSON message via WebSocket.

        Raises:
            TransportError: If no connection exists for session_id
        """
        async with self._lock:
            conn = self._connections.get(session_id)

        if not conn:
            raise TransportError(f"No connection for session {session_id}")

        try:
            await conn.websocket.send_json(message)
            logger.debug(f"Sent message to {session_id}: {message.get('type')}")
        except Exception as e:
            logger.error(f"Failed to send message to {session_id}: {e}")
            raise TransportError(f"Send failed: {e}") from e

    async def send_and_wait_delivery_ack(
        self,
        session_id: str,
        message: dict[str, Any],
        *,
        timeout: float = 0.75,
    ) -> dict[str, Any] | None:
        """Send a message and wait briefly for an optional hook delivery ack.

        The ack is best-effort and only supported by newer hooks. A timeout
        returns ``None`` so legacy fire-and-forget notify behavior is preserved.
        """
        delivery_id = message.get("delivery_id")
        if not isinstance(delivery_id, str) or not delivery_id:
            await self.send(session_id, message)
            return None

        loop = asyncio.get_running_loop()
        future: asyncio.Future[dict] = loop.create_future()
        key = (session_id, delivery_id)
        self._delivery_ack_futures[key] = future
        try:
            await self.send(session_id, message)
            return await asyncio.wait_for(future, timeout=timeout)
        except asyncio.TimeoutError:
            logger.debug(
                "Timed out waiting for delivery ack from %s (%s)",
                session_id,
                delivery_id,
            )
            return None
        finally:
            self._delivery_ack_futures.pop(key, None)

    def is_connected(self, session_id: str) -> bool:
        """Check if session has active connection."""
        return session_id in self._connections

    def get_all_sessions(self) -> list[str]:
        """Get all connected session IDs."""
        return list(self._connections.keys())

    def get_connection_pane_id(self, session_id: str) -> str | None:
        """Return the pane currently bound to a WebSocket session, if known."""
        conn = self._connections.get(session_id)
        return conn.pane_id if conn else None

    def current_websocket(self, session_id: str) -> WebSocket | None:
        """Return the live WebSocket for a session, for identity-checked disconnects."""
        conn = self._connections.get(session_id)
        return conn.websocket if conn else None

    async def ping(self, session_id: str, timeout: float = 5.0) -> dict:
        """Send a ping to a peer and wait for pong.

        Args:
            session_id: Target session
            timeout: Max seconds to wait for pong

        Returns:
            Pong data dict from the peer

        Raises:
            TimeoutError: If no pong received within timeout
            TransportError: If send fails
        """
        loop = asyncio.get_running_loop()
        future: asyncio.Future[dict] = loop.create_future()
        self._pong_futures[session_id] = future
        try:
            await self.send(session_id, {"type": "ping"})
            return await asyncio.wait_for(future, timeout=timeout)
        finally:
            self._pong_futures.pop(session_id, None)

    def resolve_pong(self, session_id: str, data: dict) -> None:
        """Resolve a pending ping future with pong data.

        Args:
            session_id: Session that sent the pong
            data: Pong message data
        """
        future = self._pong_futures.pop(session_id, None)
        if future and not future.done():
            future.set_result(data)

    def resolve_delivery_ack(self, session_id: str, data: dict[str, Any]) -> None:
        """Resolve a pending hook delivery ack future, if one exists."""
        delivery_id = data.get("delivery_id")
        if not isinstance(delivery_id, str):
            logger.warning("Delivery ack from %s missing delivery_id, dropping", session_id)
            return
        future = self._delivery_ack_futures.pop((session_id, delivery_id), None)
        if future and not future.done():
            future.set_result(data)
