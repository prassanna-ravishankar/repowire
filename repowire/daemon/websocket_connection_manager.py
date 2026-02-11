"""WebSocket connection state management.

Tracks connection status and metadata.
"""

import asyncio
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone

from fastapi import WebSocket

from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)


@dataclass
class ConnectionInfo:
    """Connection metadata."""

    session_id: str
    websocket: WebSocket
    status: PeerStatus = PeerStatus.ONLINE
    connected_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class WebSocketConnectionManager:
    """Manages WebSocket connection state."""

    def __init__(self) -> None:
        # session_id -> ConnectionInfo
        self._connections: dict[str, ConnectionInfo] = {}
        self._lock = asyncio.Lock()

    async def connect(self, session_id: str, websocket: WebSocket) -> None:
        """Register connection."""
        async with self._lock:
            self._connections[session_id] = ConnectionInfo(
                session_id=session_id,
                websocket=websocket,
            )
            logger.info(f"Connection registered: {session_id}")

    async def disconnect(self, session_id: str) -> None:
        """Unregister connection."""
        async with self._lock:
            if session_id in self._connections:
                self._connections.pop(session_id)
                logger.info(f"Connection unregistered: {session_id}")

    def is_connected(self, session_id: str) -> bool:
        """Check if connected."""
        return session_id in self._connections

    def get_status(self, session_id: str) -> PeerStatus:
        """Get connection status."""
        conn = self._connections.get(session_id)
        return conn.status if conn else PeerStatus.OFFLINE

    async def update_status(self, session_id: str, status: PeerStatus) -> bool:
        """Update status.

        Returns:
            True if updated, False if session not found
        """
        async with self._lock:
            if session_id in self._connections:
                self._connections[session_id].status = status
                logger.debug(f"Status updated for {session_id}: {status.value}")
                return True
            return False

    def get_all_connections(self) -> list[ConnectionInfo]:
        """Get all connection info."""
        return list(self._connections.values())

    def get_connection_info(self, session_id: str) -> ConnectionInfo | None:
        """Get connection info for session."""
        return self._connections.get(session_id)
