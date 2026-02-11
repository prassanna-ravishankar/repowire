"""WebSocket transport layer.

Single transport implementation using WebSocket connections.
"""

import asyncio
import logging
from typing import Any

from fastapi import WebSocket

logger = logging.getLogger(__name__)


class TransportError(Exception):
    """Transport error."""


class WebSocketTransport:
    """Transport using WebSocket connections."""

    def __init__(self) -> None:
        # session_id -> WebSocket
        self._connections: dict[str, WebSocket] = {}
        self._lock = asyncio.Lock()

    async def connect(self, session_id: str, websocket: WebSocket) -> None:
        """Register WebSocket connection.

        If a connection already exists for this session_id, close the old one.
        """
        async with self._lock:
            if session_id in self._connections:
                old_ws = self._connections[session_id]
                try:
                    await old_ws.close()
                    logger.info(f"Closed old connection for {session_id}")
                except Exception as e:
                    logger.warning(f"Failed to close old connection: {e}")

            self._connections[session_id] = websocket
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
            ws = self._connections.get(session_id)

        if not ws:
            raise TransportError(f"No connection for session {session_id}")

        try:
            await ws.send_json(message)
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
