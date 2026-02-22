"""Core logic for the Repowire daemon.

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
    from repowire.daemon.query_tracker import QueryTracker
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
        query_tracker: QueryTracker | None = None,
    ) -> None:
        """Initialize PeerManager.

        Args:
            config: Configuration instance
            message_router: Message router for sending queries/notifications
            session_mapper: Session mapper for stable peer IDs
            query_tracker: Query tracker for cancelling pending queries
        """
        self._config = config
        self._router = message_router
        self._session_mapper = session_mapper
        self._query_tracker = query_tracker

        # Peer registry: session_id -> Peer
        self._peers: dict[str, Peer] = {}
        # Secondary index: display_name -> session_id
        self._name_index: dict[str, str] = {}

        self._lock = asyncio.Lock()
        self._events: deque[dict[str, Any]] = deque(maxlen=100)

    def _add_event(self, event_type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        event_id = str(uuid4())
        self._events.append(
            {
                "id": event_id,
                "type": event_type,
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
        """Lookup peer by session_id or display_name. Must be called with lock held.

        When multiple peers share a display_name (different circles), prefers online ones.
        """
        if identifier in self._peers:
            return self._peers[identifier]
        # Scan all peers matching display_name, prefer online/busy over offline
        matches = [p for p in self._peers.values() if p.display_name == identifier]
        if not matches:
            return None
        if len(matches) == 1:
            return matches[0]
        active = [p for p in matches if p.status != PeerStatus.OFFLINE]
        return active[0] if active else matches[0]

    async def register_peer(self, peer: Peer) -> None:
        """Register a peer in the mesh.

        Indexed by session_id with secondary index on display_name.
        Only evicts old peer if same display_name AND same circle (true reconnect).
        """
        async with self._lock:
            # Handle reconnection: same name AND same circle = true reconnect
            if peer.display_name in self._name_index:
                old_session_id = self._name_index[peer.display_name]
                if old_session_id != peer.peer_id and old_session_id in self._peers:
                    old_peer = self._peers[old_session_id]
                    if old_peer.circle == peer.circle:
                        del self._peers[old_session_id]
                    # Different circle: don't evict, they're distinct peers

            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)

            self._peers[peer.peer_id] = peer
            self._name_index[peer.display_name] = peer.peer_id

            logger.info(f"Peer registered: {peer.display_name} ({peer.peer_id})")

    async def unregister_peer(self, identifier: str, circle: str | None = None) -> bool:
        """Unregister a peer from the mesh.

        Args:
            identifier: Either session_id or display_name
            circle: Optional circle filter to disambiguate same-name peers

        Returns:
            True if peer was found and removed
        """
        async with self._lock:
            # Try as session_id first (always unambiguous)
            if identifier in self._peers:
                peer = self._peers.pop(identifier)
                self._name_index.pop(peer.display_name, None)
                logger.info(f"Peer unregistered: {peer.display_name} ({identifier})")
                return True

            # Try as display_name — with optional circle filter
            if circle:
                # Find the specific peer matching name + circle
                for sid, peer in list(self._peers.items()):
                    if peer.display_name == identifier and peer.circle == circle:
                        self._peers.pop(sid)
                        if self._name_index.get(identifier) == sid:
                            self._name_index.pop(identifier, None)
                        logger.info(f"Peer unregistered: {identifier} in {circle} ({sid})")
                        return True
                return False

            if identifier in self._name_index:
                session_id = self._name_index.pop(identifier)
                self._peers.pop(session_id, None)
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
        self._config = load_config()

        async with self._lock:
            result: list[Peer] = []
            mappings = self._session_mapper.get_all_mappings()

            for session_id, mapping in mappings.items():
                if session_id in self._peers:
                    result.append(self._peers[session_id])
                else:
                    result.append(
                        Peer(
                            peer_id=session_id,
                            display_name=mapping.display_name,
                            path=mapping.path or "",
                            machine="unknown",
                            backend=mapping.backend,
                            circle=mapping.circle,
                            status=PeerStatus.OFFLINE,
                            metadata={},
                        )
                    )

            return result

    def _get_peer_config(self, name: str) -> PeerConfig | None:
        """Get peer config by name."""
        return self._config.peers.get(name)

    def _check_circle_access(self, from_peer: str, to_peer: str, bypass: bool) -> None:
        """Check if from_peer can access to_peer based on circles.

        Raises:
            ValueError: If access not allowed
        """
        if bypass:
            return

        from_config = self._get_peer_config(from_peer)
        to_config = self._get_peer_config(to_peer)

        if not from_config or not to_config:
            return

        from_circle = from_config.circle or "default"
        to_circle = to_config.circle or "default"

        if from_circle != to_circle:
            raise ValueError(
                f"Circle boundary: {from_peer} ({from_circle}) "
                f"cannot access {to_peer} ({to_circle})"
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
        peer = await self.get_peer(to_peer)
        if not peer:
            raise ValueError(f"Unknown peer: {to_peer}")

        self._check_circle_access(from_peer, to_peer, bypass_circle)

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
            response = await self._router.send_query(
                from_peer=from_peer,
                to_session_id=peer.peer_id,
                to_peer_name=peer.display_name,
                text=formatted_query,
                timeout=timeout,
            )

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

        Raises:
            ValueError: If peer not found or circle boundary violated
        """
        peer = await self.get_peer(to_peer)
        if not peer:
            raise ValueError(f"Unknown peer: {to_peer}")

        self._check_circle_access(from_peer, to_peer, bypass_circle)

        self._add_event(
            "notification",
            {"from": from_peer, "to": to_peer, "text": text},
        )

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

        Returns:
            List of peer names that received the broadcast
        """
        self._add_event(
            "broadcast",
            {"from": from_peer, "text": text, "exclude": exclude},
        )

        # Determine sender's circle for filtering
        from_circle: str | None = None
        if not bypass_circle:
            async with self._lock:
                from_peer_obj = self._lookup_peer_unlocked(from_peer)
                if from_peer_obj:
                    from_circle = from_peer_obj.circle

        # Build exclude set of session IDs
        exclude_names = set(exclude or [])
        exclude_names.add(from_peer)

        exclude_session_ids: set[str] = set()
        async with self._lock:
            for name in exclude_names:
                sid = self._name_index.get(name)
                if sid:
                    exclude_session_ids.add(sid)

            # Circle filtering: exclude peers in different circles
            if from_circle and not bypass_circle:
                for sid, peer in self._peers.items():
                    if peer.circle != from_circle:
                        exclude_session_ids.add(sid)

        sent_session_ids = await self._router.broadcast(
            from_peer=from_peer,
            text=text,
            exclude=exclude_session_ids,
        )

        async with self._lock:
            return [self._peers[sid].display_name for sid in sent_session_ids if sid in self._peers]

    async def update_peer_status(self, identifier: str, status: PeerStatus) -> None:
        """Update peer status."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                peer.status = status
                peer.last_seen = datetime.now(timezone.utc)

    async def set_peer_circle(self, identifier: str, circle: str) -> None:
        """Update peer's circle."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                old_circle = peer.circle
                peer.circle = circle
                logger.info(f"Peer {peer.display_name} moved from {old_circle} to {circle}")

    def resolve_hook_response(self, correlation_id: str, response: str) -> None:
        """Resolve a hook response via QueryTracker.

        In the unified architecture, responses normally arrive via WebSocket.
        This method handles legacy hooks that POST responses to /hook/response.
        """
        if self._query_tracker:
            resolved = self._query_tracker.resolve_query(correlation_id, response)
            if resolved:
                logger.info(f"Hook response resolved: {correlation_id[:8]}...")
                return

        logger.warning(
            f"Hook response could not be resolved: {correlation_id[:8]}... "
            "(no matching pending query)"
        )

    def resolve_circle(self, peer_config: PeerConfig) -> str:
        """Resolve circle for a peer. Returns circle from config or "default"."""
        return peer_config.circle or "default"

    async def mark_offline(self, identifier: str) -> int:
        """Mark peer offline and cancel pending queries.

        Args:
            identifier: Peer session_id or display_name

        Returns:
            Number of cancelled queries
        """
        await self.update_peer_status(identifier, PeerStatus.OFFLINE)

        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                return 0
            session_id = peer.peer_id

        cancelled = 0
        if self._query_tracker:
            cancelled = self._query_tracker.cancel_queries_to_peer(session_id)

        logger.info(f"Marked {identifier} offline, cancelled {cancelled} queries")
        return cancelled
