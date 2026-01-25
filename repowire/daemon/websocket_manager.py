"""WebSocket connection manager for OpenCode plugins."""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

from fastapi import WebSocket, WebSocketDisconnect

from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)


@dataclass
class PluginConnection:
    """Represents a connected OpenCode plugin."""

    websocket: WebSocket
    peer_name: str
    path: str
    status: PeerStatus = PeerStatus.ONLINE
    session_id: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)
    connected_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class PendingQuery:
    """Tracks a pending query waiting for response."""

    correlation_id: str
    from_peer: str
    to_peer: str
    future: asyncio.Future[str]
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class WebSocketManager:
    """Manages WebSocket connections from OpenCode plugins."""

    def __init__(self) -> None:
        self._connections: dict[str, PluginConnection] = {}  # peer_name -> connection
        self._pending_queries: dict[str, PendingQuery] = {}  # correlation_id -> query
        self._lock = asyncio.Lock()

    async def connect(
        self,
        websocket: WebSocket,
        peer_name: str,
        path: str,
        metadata: dict[str, Any] | None = None,
    ) -> PluginConnection:
        """Register a new plugin connection.

        Args:
            websocket: The WebSocket connection
            peer_name: Name of the peer
            path: Path to the peer's project directory
            metadata: Optional metadata (branch, etc.)

        Returns:
            The created PluginConnection
        """
        async with self._lock:
            # Disconnect existing connection if any
            if peer_name in self._connections:
                old_conn = self._connections[peer_name]
                try:
                    await old_conn.websocket.close()
                except Exception as e:
                    logger.warning(
                        f"Failed to close existing connection for {peer_name}, "
                        f"proceeding with new connection: {e}"
                    )

            connection = PluginConnection(
                websocket=websocket,
                peer_name=peer_name,
                path=path,
                metadata=metadata or {},
            )
            self._connections[peer_name] = connection
            logger.info(f"Plugin connected: {peer_name} from {path}")
            return connection

    async def disconnect(self, peer_name: str) -> None:
        """Unregister a plugin connection.

        Args:
            peer_name: Name of the peer to disconnect
        """
        async with self._lock:
            if peer_name in self._connections:
                del self._connections[peer_name]
                logger.info(f"Plugin disconnected: {peer_name}")

            # Cancel any pending queries to this peer
            cancelled = []
            for cid, query in list(self._pending_queries.items()):
                if query.to_peer == peer_name:
                    if not query.future.done():
                        query.future.set_exception(
                            ConnectionError(f"Peer {peer_name} disconnected")
                        )
                    cancelled.append(cid)

            for cid in cancelled:
                del self._pending_queries[cid]

            if cancelled:
                logger.info(f"Cancelled {len(cancelled)} pending queries to {peer_name}")

    def get_connection(self, peer_name: str) -> PluginConnection | None:
        """Get a connection by peer name."""
        return self._connections.get(peer_name)

    def is_connected(self, peer_name: str) -> bool:
        """Check if a peer is connected via WebSocket."""
        return peer_name in self._connections

    def get_peer_status(self, peer_name: str) -> PeerStatus:
        """Get the status of a connected peer."""
        conn = self._connections.get(peer_name)
        if not conn:
            return PeerStatus.OFFLINE
        return conn.status

    async def update_status(self, peer_name: str, status: PeerStatus) -> bool:
        """Update peer status.

        Args:
            peer_name: Name of the peer
            status: New status

        Returns:
            True if peer was found and updated
        """
        async with self._lock:
            if peer_name in self._connections:
                self._connections[peer_name].status = status
                logger.debug(f"Plugin status update: {peer_name} -> {status.value}")
                return True
            logger.debug(f"Status update for {peer_name} skipped: not connected")
            return False

    async def update_session_id(self, peer_name: str, session_id: str) -> bool:
        """Update peer's active session ID.

        Args:
            peer_name: Name of the peer
            session_id: New session ID

        Returns:
            True if peer was found and updated
        """
        async with self._lock:
            if peer_name in self._connections:
                self._connections[peer_name].session_id = session_id
                logger.debug(f"Plugin session update: {peer_name} -> {session_id}")
                return True
            logger.debug(f"Session update for {peer_name} skipped: not connected")
            return False

    async def send_query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = 120.0,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not connected
            TimeoutError: If no response within timeout
            ConnectionError: If connection lost during send
        """
        correlation_id = str(uuid4())
        future: asyncio.Future[str] = asyncio.get_running_loop().create_future()

        query = PendingQuery(
            correlation_id=correlation_id,
            from_peer=from_peer,
            to_peer=to_peer,
            future=future,
        )

        # Acquire lock to atomically check connection and register query
        async with self._lock:
            conn = self._connections.get(to_peer)
            if not conn:
                raise ValueError(f"Peer {to_peer} is not connected")
            self._pending_queries[correlation_id] = query

        try:
            # Send query to plugin via WebSocket
            message = {
                "type": "query",
                "correlation_id": correlation_id,
                "from_peer": from_peer,
                "text": text,
            }
            try:
                await conn.websocket.send_json(message)
            except (WebSocketDisconnect, ConnectionError) as e:
                raise ConnectionError(f"Lost connection to {to_peer}: {e}") from e
            logger.debug(f"Sent query {correlation_id} to {to_peer}")

            # Wait for response with timeout
            response = await asyncio.wait_for(future, timeout=timeout)
            return response

        except asyncio.TimeoutError:
            logger.warning(f"Query {correlation_id} to {to_peer} timed out")
            raise TimeoutError(f"Query to {to_peer} timed out after {timeout}s")

        finally:
            async with self._lock:
                self._pending_queries.pop(correlation_id, None)

    async def resolve_query(
        self,
        correlation_id: str,
        response_text: str,
    ) -> bool:
        """Resolve a pending query with a response.

        Args:
            correlation_id: The query's correlation ID
            response_text: The response text

        Returns:
            True if query was found and resolved
        """
        async with self._lock:
            query = self._pending_queries.get(correlation_id)
            if not query:
                logger.warning(f"No pending query for correlation_id: {correlation_id}")
                return False

            if not query.future.done():
                query.future.set_result(response_text)
                logger.debug(f"Resolved query {correlation_id}")
                return True
            return False

    async def resolve_query_error(
        self,
        correlation_id: str,
        error: str,
    ) -> bool:
        """Resolve a pending query with an error.

        Args:
            correlation_id: The query's correlation ID
            error: The error message

        Returns:
            True if query was found and resolved
        """
        async with self._lock:
            query = self._pending_queries.get(correlation_id)
            if not query:
                logger.warning(f"No pending query for correlation_id: {correlation_id}")
                return False

            if not query.future.done():
                query.future.set_exception(ValueError(error))
                logger.debug(f"Resolved query {correlation_id} with error: {error}")
                return True
            return False

    async def send_notification(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
    ) -> bool:
        """Send a notification to a peer (fire-and-forget).

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Notification text

        Returns:
            True if message was sent
        """
        # Snapshot connection under lock to avoid race conditions
        async with self._lock:
            conn = self._connections.get(to_peer)
            if not conn:
                logger.debug(f"Cannot send notification to {to_peer}: peer not connected")
                return False
            websocket = conn.websocket

        message = {
            "type": "notify",
            "from_peer": from_peer,
            "text": text,
        }

        try:
            await websocket.send_json(message)
            return True
        except (WebSocketDisconnect, ConnectionError) as e:
            logger.warning(f"Connection lost sending notification to {to_peer}: {e}")
            return False
        except Exception as e:
            logger.error(f"Unexpected error sending notification to {to_peer}: {e}", exc_info=True)
            return False

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: set[str] | None = None,
    ) -> list[str]:
        """Broadcast a message to all connected peers.

        Args:
            from_peer: Name of the sending peer
            text: Broadcast text
            exclude: Peer names to exclude

        Returns:
            List of peer names that received the message
        """
        excluded = exclude or set()
        excluded.add(from_peer)  # Don't send to self

        message = {
            "type": "broadcast",
            "from_peer": from_peer,
            "text": text,
        }

        # Snapshot connections under lock to avoid race conditions
        async with self._lock:
            connections_snapshot = list(self._connections.items())

        sent_to: list[str] = []
        for peer_name, conn in connections_snapshot:
            if peer_name in excluded:
                continue

            try:
                await conn.websocket.send_json(message)
                sent_to.append(peer_name)
            except (WebSocketDisconnect, ConnectionError) as e:
                logger.warning(f"Failed to broadcast to {peer_name} (connection issue): {e}")
            except Exception as e:
                logger.error(f"Unexpected error broadcasting to {peer_name}: {e}", exc_info=True)

        return sent_to

    async def cancel_queries_to_peer(self, peer_name: str) -> int:
        """Cancel all pending queries to a peer.

        Args:
            peer_name: Name of the peer

        Returns:
            Number of queries cancelled
        """
        async with self._lock:
            cancelled = 0
            for cid, query in list(self._pending_queries.items()):
                if query.to_peer == peer_name:
                    if not query.future.done():
                        query.future.set_exception(
                            ConnectionError(f"Peer {peer_name} went offline")
                        )
                    del self._pending_queries[cid]
                    cancelled += 1
            return cancelled

    def get_all_connections(self) -> list[PluginConnection]:
        """Get all active connections."""
        return list(self._connections.values())


# Global instance - will be initialized in app.py
ws_manager: WebSocketManager | None = None


def get_ws_manager() -> WebSocketManager:
    """Get the global WebSocket manager instance."""
    global ws_manager
    if ws_manager is None:
        ws_manager = WebSocketManager()
    return ws_manager


def init_ws_manager() -> WebSocketManager:
    """Initialize the global WebSocket manager."""
    global ws_manager
    ws_manager = WebSocketManager()
    return ws_manager
