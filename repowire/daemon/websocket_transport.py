"""WebSocket transport layer.

Handles WebSocket connections, status tracking, and message delivery.
"""

import asyncio
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from fastapi import WebSocket

from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)


class TransportError(Exception):
    """Transport error."""


@dataclass
class ConnectionInfo:
    """Connection metadata."""

    session_id: str
    websocket: WebSocket
    status: PeerStatus = PeerStatus.ONLINE
    connected_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class WebSocketTransport:
    """Transport using WebSocket connections.

    Manages both the raw WebSocket connections and their associated metadata
    (status, connected_at).
    """

    def __init__(self) -> None:
        self._connections: dict[str, ConnectionInfo] = {}
        self._lock = asyncio.Lock()

    async def connect(self, session_id: str, websocket: WebSocket) -> None:
        """Register WebSocket connection.

        If a connection already exists for this session_id, close the old one.
        """
        async with self._lock:
            if session_id in self._connections:
                old_ws = self._connections[session_id].websocket
                try:
                    await old_ws.close()
                    logger.info(f"Closed old connection for {session_id}")
                except Exception as e:
                    logger.warning(f"Failed to close old connection: {e}")

            self._connections[session_id] = ConnectionInfo(
                session_id=session_id,
                websocket=websocket,
            )
            logger.info(f"Registered connection for {session_id}")

    async def disconnect(self, session_id: str) -> None:
        """Unregister WebSocket connection."""
        async with self._lock:
            if session_id in self._connections:
                self._connections.pop(session_id)
                logger.info(f"Unregistered connection for {session_id}")

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

    def is_connected(self, session_id: str) -> bool:
        """Check if session has active connection."""
        return session_id in self._connections

    def get_all_sessions(self) -> list[str]:
        """Get all connected session IDs."""
        return list(self._connections.keys())

    def get_status(self, session_id: str) -> PeerStatus:
        """Get connection status."""
        conn = self._connections.get(session_id)
        return conn.status if conn else PeerStatus.OFFLINE

    async def update_status(self, session_id: str, status: PeerStatus) -> bool:
        """Update connection status.

        Returns:
            True if updated, False if session not found
        """
        async with self._lock:
            conn = self._connections.get(session_id)
            if conn:
                conn.status = status
                logger.debug(f"Status updated for {session_id}: {status.value}")
                return True
            return False

    def get_connection_info(self, session_id: str) -> ConnectionInfo | None:
        """Get connection info for session."""
        return self._connections.get(session_id)

    def get_all_connections(self) -> list[ConnectionInfo]:
        """Get all connection info."""
        return list(self._connections.values())
