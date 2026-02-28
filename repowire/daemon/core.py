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

from repowire.config.models import DEFAULT_QUERY_TIMEOUT, Config
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

        # Peer registry: session_id -> Peer (single source of truth)
        self._peers: dict[str, Peer] = {}

        self._lock = asyncio.Lock()
        self._events: deque[dict[str, Any]] = deque(maxlen=100)

    def add_event(self, event_type: str, data: dict[str, Any]) -> str:
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

    def _lookup_peer_unlocked(self, identifier: str, circle: str | None = None) -> Peer | None:
        """Lookup peer by session_id or display_name. Must be called with lock held.

        When multiple peers share a display_name (different circles), filters by
        circle if provided, otherwise prefers online ones.
        """
        if identifier in self._peers:
            return self._peers[identifier]
        # Scan all peers matching display_name
        matches = [p for p in self._peers.values() if p.display_name == identifier]
        if not matches:
            return None
        # Filter by circle if specified
        if circle:
            matches = [p for p in matches if p.circle == circle]
            if not matches:
                return None
        if len(matches) == 1:
            return matches[0]
        active = [p for p in matches if p.status != PeerStatus.OFFLINE]
        return active[0] if active else matches[0]

    async def register_peer(self, peer: Peer) -> None:
        """Register a peer in the mesh.

        Indexed by session_id. Only evicts old peer if same
        (display_name, circle, backend) — a true reconnect.
        """
        async with self._lock:
            # Evict stale entries for the same (display_name, backend):
            # - same circle + different sid = true reconnect
            # - OFFLINE + any circle = ghost from a dead hook with stale circle
            for old_sid, old_peer in list(self._peers.items()):
                if (
                    old_peer.display_name == peer.display_name
                    and old_peer.backend == peer.backend
                    and old_sid != peer.peer_id
                    and (old_peer.circle == peer.circle or old_peer.status == PeerStatus.OFFLINE)
                ):
                    del self._peers[old_sid]

            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)

            self._peers[peer.peer_id] = peer

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
                logger.info(f"Peer unregistered: {peer.display_name} ({identifier})")
                return True

            # Try as display_name — with optional circle filter
            for sid, peer in list(self._peers.items()):
                if peer.display_name == identifier:
                    if circle and peer.circle != circle:
                        continue
                    self._peers.pop(sid)
                    logger.info(f"Peer unregistered: {identifier} ({sid})")
                    return True

            return False

    async def get_peer(self, identifier: str, circle: str | None = None) -> Peer | None:
        """Get a peer by session_id or display_name.

        Args:
            identifier: Either session_id (e.g., 'repow-dev-a1b2c3d4') or display_name
            circle: Optional circle filter to disambiguate same-name peers

        Returns:
            Peer if found, None otherwise
        """
        async with self._lock:
            return self._lookup_peer_unlocked(identifier, circle=circle)

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers.

        Combines in-memory peers with session mappings.
        """
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

    def _check_circle_access_by_peers(
        self, from_obj: Peer | None, to_obj: Peer | None, bypass: bool
    ) -> None:
        """Check circle access given already-resolved Peer objects. Must hold lock.

        Raises:
            ValueError: If access not allowed
        """
        if bypass:
            return

        if not from_obj or not to_obj:
            return  # Unknown peer = no enforcement (CLI callers, bypass already handled)

        if from_obj.circle != to_obj.circle:
            raise ValueError(
                f"Circle boundary: {from_obj.display_name} ({from_obj.circle}) "
                f"cannot access {to_obj.display_name} ({to_obj.circle})"
            )

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = DEFAULT_QUERY_TIMEOUT,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds
            bypass_circle: If True, bypass circle restrictions (CLI mode)
            circle: Optional circle filter to disambiguate same-name peers

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not found or circle boundary violated
            TimeoutError: If no response within timeout
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(to_peer, circle=circle)
            if not peer:
                raise ValueError(f"Unknown peer: {to_peer}")
            from_peer_obj = self._lookup_peer_unlocked(
                from_peer, circle=peer.circle
            ) or self._lookup_peer_unlocked(from_peer)
            self._check_circle_access_by_peers(from_peer_obj, peer, bypass_circle)
            peer_id = peer.peer_id
            peer_name = peer.display_name

        formatted_query = (
            f"[Repowire Query from @{from_peer}]\n"
            f"{text}\n\n"
            f"IMPORTANT: Respond directly in your message. Do NOT use ask_peer() to reply - "
            f"your response is automatically captured and returned to {from_peer}."
        )

        query_event_id = self.add_event(
            "query",
            {"from": from_peer, "to": to_peer, "text": text, "status": "pending"},
        )

        try:
            response = await self._router.send_query(
                from_peer=from_peer,
                to_session_id=peer_id,
                to_peer_name=peer_name,
                text=formatted_query,
                timeout=timeout,
            )

            self._update_event(query_event_id, {"status": "success"})
            self.add_event(
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
        circle: str | None = None,
    ) -> None:
        """Send a notification to a peer (fire-and-forget).

        Raises:
            ValueError: If peer not found or circle boundary violated
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(to_peer, circle=circle)
            if not peer:
                raise ValueError(f"Unknown peer: {to_peer}")
            from_peer_obj = self._lookup_peer_unlocked(
                from_peer, circle=peer.circle
            ) or self._lookup_peer_unlocked(from_peer)
            self._check_circle_access_by_peers(from_peer_obj, peer, bypass_circle)
            peer_id = peer.peer_id
            peer_name = peer.display_name

        self.add_event(
            "notification",
            {"from": from_peer, "to": to_peer, "text": text},
        )

        await self._router.send_notification(
            from_peer=from_peer,
            to_session_id=peer_id,
            to_peer_name=peer_name,
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
        self.add_event(
            "broadcast",
            {"from": from_peer, "text": text, "exclude": exclude},
        )

        exclude_names = set(exclude or [])
        exclude_names.add(from_peer)

        exclude_session_ids: set[str] = set()
        async with self._lock:
            # Resolve exclude names to session IDs; track sender for circle filtering
            from_peer_obj: Peer | None = None
            for name in exclude_names:
                peer = self._lookup_peer_unlocked(name)
                if peer:
                    exclude_session_ids.add(peer.peer_id)
                    if name == from_peer:
                        from_peer_obj = peer

            # Circle filtering: exclude peers outside sender's circle
            if not bypass_circle and from_peer_obj:
                from_circle = from_peer_obj.circle
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
            else:
                logger.warning(
                    "update_peer_status: peer not found: %s (status=%s not applied)",
                    identifier,
                    status.value,
                )

    async def set_peer_circle(self, identifier: str, circle: str) -> None:
        """Update peer's circle."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                old_circle = peer.circle
                peer.circle = circle
                logger.info(f"Peer {peer.display_name} moved from {old_circle} to {circle}")
            else:
                logger.warning(
                    "set_peer_circle: peer not found: %s (circle=%s not applied)",
                    identifier,
                    circle,
                )

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
