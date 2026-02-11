"""Simplified core logic for the Repowire daemon.

Uses unified WebSocket architecture with MessageRouter for all message delivery.
"""

from __future__ import annotations

import asyncio
import logging
from collections import deque
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any
from uuid import uuid4

from repowire.config.models import Config, PeerConfig, load_config
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.daemon.message_router import MessageRouter
    from repowire.daemon.session_mapper import SessionMapper

logger = logging.getLogger(__name__)


class PeerManager:
    """Manages peer registry and delegates message routing to MessageRouter.

    Thread-safe with asyncio locks.
    """

    def __init__(
        self,
        config: Config,
        message_router: MessageRouter,
        session_mapper: SessionMapper,
    ) -> None:
        """Initialize PeerManager.

        Args:
            config: Configuration instance
            message_router: Message router for sending queries/notifications
            session_mapper: Session mapper for stable peer IDs
        """
        self._config = config
        self._router = message_router
        self._session_mapper = session_mapper

        # Peer registry
        # session_id -> Peer
        self._peers: dict[str, Peer] = {}
        # display_name -> session_id (for lookup)
        self._name_index: dict[str, str] = {}

        self._lock = asyncio.Lock()
        self._events: deque[dict[str, Any]] = deque(maxlen=100)

    @property
    def backend_name(self) -> str:
        """Get backend name (for health check)."""
        return "unified-websocket"

    def _add_event(self, type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        event_id = str(uuid4())
        self._events.append(
            {
                "id": event_id,
                "type": type,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                **data,
            }
        )
        return event_id

    def _update_event(self, event_id: str, updates: dict[str, Any]) -> bool:
        """Update an existing event by ID."""
        for event in self._events:
            if event["id"] == event_id:
                event.update(updates)
                return True
        return False

    def get_events(self) -> list[dict[str, Any]]:
        """Get the last 100 events."""
        return list(self._events)

    async def start(self) -> None:
        """Start the peer manager."""
        logger.info("PeerManager started with unified WebSocket backend")

    async def stop(self) -> None:
        """Stop the peer manager."""
        logger.info("PeerManager stopped")

    def _lookup_peer_unlocked(self, identifier: str) -> Peer | None:
        """Lookup peer by session_id or display_name. Must be called with lock held."""
        # Try as session_id first
        if identifier in self._peers:
            return self._peers[identifier]
        # Try as display_name
        if identifier in self._name_index:
            session_id = self._name_index[identifier]
            return self._peers.get(session_id)
        return None

    async def register_peer(self, peer: Peer) -> None:
        """Register a peer in the mesh.

        Indexed by session_id with secondary index on display_name.
        """
        async with self._lock:
            # Check for existing peer with same name but different ID (reconnection)
            if peer.display_name in self._name_index:
                old_session_id = self._name_index[peer.display_name]
                if old_session_id != peer.peer_id and old_session_id in self._peers:
                    del self._peers[old_session_id]

            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)

            # Primary index by session_id
            self._peers[peer.peer_id] = peer
            # Secondary index by display_name
            self._name_index[peer.display_name] = peer.peer_id

            logger.info(f"Peer registered: {peer.display_name} ({peer.peer_id})")

    async def unregister_peer(self, identifier: str) -> bool:
        """Unregister a peer from the mesh.

        Args:
            identifier: Either session_id or display_name

        Returns:
            True if peer was found and removed
        """
        async with self._lock:
            # Try as session_id first
            if identifier in self._peers:
                peer = self._peers[identifier]
                del self._peers[identifier]
                # Remove from name index
                if peer.display_name in self._name_index:
                    del self._name_index[peer.display_name]
                logger.info(f"Peer unregistered: {peer.display_name} ({identifier})")
                return True

            # Try as display_name
            if identifier in self._name_index:
                session_id = self._name_index[identifier]
                if session_id in self._peers:
                    peer = self._peers[session_id]
                    del self._peers[session_id]
                del self._name_index[identifier]
                logger.info(f"Peer unregistered: {identifier} ({session_id})")
                return True

            return False

    async def get_peer(self, identifier: str) -> Peer | None:
        """Get a peer by session_id or display_name.

        Args:
            identifier: Either session_id (e.g., 'repow-dev-a1b2c3d4') or display_name

        Returns:
            Peer if found, None otherwise
        """
        async with self._lock:
            return self._lookup_peer_unlocked(identifier)

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers.

        Combines in-memory peers with session mappings.
        """
        # Reload config for fresh peer info
        self._config = load_config()

        async with self._lock:
            result: list[Peer] = []

            # Get all session mappings
            mappings = self._session_mapper.get_all_mappings()

            for session_id, mapping in mappings.items():
                # Check if peer is in memory (connected)
                if session_id in self._peers:
                    result.append(self._peers[session_id])
                else:
                    # Create offline peer from mapping
                    peer = Peer(
                        peer_id=session_id,
                        display_name=mapping.display_name,
                        path=mapping.path or "",
                        machine="unknown",
                        backend=mapping.backend,
                        circle=mapping.circle,
                        status=PeerStatus.OFFLINE,
                        metadata={},
                    )
                    result.append(peer)

            return result

    def _get_peer_config(self, name: str) -> PeerConfig | None:
        """Get peer config by name."""
        return self._config.peers.get(name)

    def _check_circle_access(
        self, from_peer: str, to_peer: str, bypass: bool
    ) -> None:
        """Check if from_peer can access to_peer based on circles.

        Args:
            from_peer: Sender name
            to_peer: Recipient name
            bypass: If True, bypass circle restrictions

        Raises:
            ValueError: If access not allowed
        """
        if bypass:
            return

        from_config = self._get_peer_config(from_peer)
        to_config = self._get_peer_config(to_peer)

        if not from_config or not to_config:
            return  # Allow if config not found

        from_circle = from_config.circle or "default"
        to_circle = to_config.circle or "default"

        if from_circle != to_circle:
            raise ValueError(
                f"Circle boundary: {from_peer} ({from_circle}) cannot access {to_peer} ({to_circle})"
            )

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = 120.0,
        bypass_circle: bool = False,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds
            bypass_circle: If True, bypass circle restrictions (CLI mode)

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not found or circle boundary violated
            TimeoutError: If no response within timeout
        """
        # Look up peer
        peer = await self.get_peer(to_peer)
        if not peer:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check circle access
        self._check_circle_access(from_peer, to_peer, bypass_circle)

        # Format the query with sender info
        formatted_query = (
            f"[Repowire Query from @{from_peer}]\n"
            f"{text}\n\n"
            f"IMPORTANT: Respond directly in your message. Do NOT use ask_peer() to reply - "
            f"your response is automatically captured and returned to {from_peer}."
        )

        query_event_id = self._add_event(
            "query",
            {"from": from_peer, "to": to_peer, "text": text, "status": "pending"},
        )

        try:
            # Delegate to MessageRouter
            response = await self._router.send_query(
                from_peer=from_peer,
                to_session_id=peer.peer_id,
                to_peer_name=peer.display_name,
                text=formatted_query,
                timeout=timeout,
            )

            # Update query event to success
            self._update_event(query_event_id, {"status": "success"})
            self._add_event(
                "response",
                {
                    "from": to_peer,
                    "to": from_peer,
                    "text": response[:100] + "..." if len(response) > 100 else response,
                    "correlation_id": query_event_id,
                },
            )

            return response

        except TimeoutError:
            self._update_event(query_event_id, {"status": "timeout"})
            raise

        except Exception as e:
            self._update_event(query_event_id, {"status": "error", "error": str(e)})
            raise

    async def notify(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        bypass_circle: bool = False,
    ) -> None:
        """Send a notification to a peer (fire-and-forget).

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Notification text
            bypass_circle: If True, bypass circle restrictions

        Raises:
            ValueError: If peer not found or circle boundary violated
        """
        # Look up peer
        peer = await self.get_peer(to_peer)
        if not peer:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check circle access
        self._check_circle_access(from_peer, to_peer, bypass_circle)

        self._add_event(
            "notification",
            {"from": from_peer, "to": to_peer, "text": text},
        )

        # Delegate to MessageRouter
        await self._router.send_notification(
            from_peer=from_peer,
            to_session_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text=text,
        )

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> list[str]:
        """Broadcast a message to all peers.

        Args:
            from_peer: Name of the sending peer
            text: Broadcast text
            exclude: Optional list of peer names to exclude
            bypass_circle: If True, broadcast to all circles (CLI mode)

        Returns:
            List of peer names that received the broadcast
        """
        self._add_event(
            "broadcast",
            {"from": from_peer, "text": text, "exclude": exclude},
        )

        # Build exclude set of session IDs
        exclude_names = set(exclude or [])
        exclude_names.add(from_peer)  # Never send to sender

        exclude_session_ids: set[str] = set()
        async with self._lock:
            for name in exclude_names:
                sid = self._name_index.get(name)
                if sid:
                    exclude_session_ids.add(sid)

        # Delegate to MessageRouter
        sent_session_ids = await self._router.broadcast(
            from_peer=from_peer,
            text=text,
            exclude=exclude_session_ids,
        )

        # Return display names of recipients
        async with self._lock:
            return [
                self._peers[sid].display_name
                for sid in sent_session_ids
                if sid in self._peers
            ]

    async def _get_session_id_for_name(self, name: str) -> str | None:
        """Get session_id for a display_name."""
        async with self._lock:
            return self._name_index.get(name)

    async def update_peer_status(self, identifier: str, status: PeerStatus) -> None:
        """Update peer status.

        Args:
            identifier: Either session_id or display_name
            status: New status
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                peer.status = status
                peer.last_seen = datetime.now(timezone.utc)

    async def update_peer_session_id(self, identifier: str, session_id: str) -> None:
        """Update peer's Claude session ID (metadata).

        Args:
            identifier: Either session_id or display_name
            session_id: Claude session ID
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                peer.metadata = peer.metadata or {}
                peer.metadata["session_id"] = session_id

    async def set_peer_circle(self, identifier: str, circle: str) -> None:
        """Update peer's circle.

        Args:
            identifier: Either session_id or display_name
            circle: New circle name
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                old_circle = peer.circle
                peer.circle = circle
                logger.info(
                    f"Peer {peer.display_name} moved from {old_circle} to {circle}"
                )

    async def register_peer_with_config(
        self,
        peer: Peer,
        path: str,
        opencode_url: str | None = None,
        circle: str | None = None,
    ) -> None:
        """Register peer and update config atomically (for WebSocket connections).

        Args:
            peer: Peer to register
            path: Peer path
            opencode_url: Optional OpenCode URL
            circle: Optional circle override
        """
        # Register in memory
        await self.register_peer(peer)

        # Update config
        self._config.peers[peer.display_name] = PeerConfig(
            name=peer.display_name,
            path=path,
            opencode_url=opencode_url,
            circle=circle or peer.circle,
        )
        self._config.save()

    def resolve_hook_response(self, correlation_id: str, response: str) -> None:
        """Legacy method for backward compatibility with old hooks.

        In the unified WebSocket architecture, responses come via WebSocket,
        not via HTTP POST. This method is deprecated but kept for compatibility.
        """
        logger.warning(
            "resolve_hook_response called but is deprecated in unified WebSocket architecture. "
            f"Correlation ID: {correlation_id[:8]}..."
        )

    def resolve_circle(self, peer_config: PeerConfig) -> str:
        """Resolve circle for a peer.

        Uses circle from config, or defaults to "default".

        Args:
            peer_config: Peer configuration

        Returns:
            Circle name
        """
        return peer_config.circle or "default"

    async def mark_offline(self, identifier: str) -> int:
        """Mark peer offline and cancel pending queries.

        Args:
            identifier: Peer session_id or display_name

        Returns:
            Number of cancelled queries
        """
        # Update status
        await self.update_peer_status(identifier, PeerStatus.OFFLINE)

        # Get session_id for cancelling queries
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                return 0
            session_id = peer.peer_id

        # Cancel queries (QueryTracker handles this)
        # For now, return 0 as we don't track individual query counts
        # The QueryTracker will handle cleanup when connection is lost
        logger.info(f"Marked {identifier} offline")
        return 0
