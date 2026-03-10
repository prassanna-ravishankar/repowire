"""Centralized query tracking for the Repowire daemon.

The QueryTracker manages pending queries, providing:
- Unified query lifecycle management (register, resolve, cancel)
- Correlation ID based request/response matching
- Peer-based query cancellation (e.g., when a peer goes offline)

Provides a single unified tracker for all query lifecycle management.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from uuid import uuid4

from repowire.protocol.errors import PeerDisconnectedError

logger = logging.getLogger(__name__)


@dataclass
class PendingQuery:
    """A query waiting for a response.

    Attributes:
        correlation_id: Unique identifier for request/response matching
        from_peer: Display name of the sending peer (for message formatting)
        to_peer_id: peer_id of the target peer (for routing)
        to_peer_name: Display name of the target peer (for error messages)
        query_text: The original query text
        created_at: When the query was created
        future: Asyncio Future that will be resolved with the response
    """

    correlation_id: str
    from_peer: str
    to_peer_id: str
    to_peer_name: str
    query_text: str
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    future: asyncio.Future[str] = field(default_factory=lambda: asyncio.Future())


class QueryTracker:
    """Centralized tracking for pending queries.

    Provides unified query lifecycle management independent of the agent type.
    All methods are synchronous and run atomically within the asyncio event loop.

    Usage:
        tracker = QueryTracker()

        # Register a query (before sending to peer)
        correlation_id = tracker.register_query(
            from_peer="frontend",
            to_peer_id="repow-dev-a1b2c3d4",
            to_peer_name="backend",
            query_text="What's the API status?"
        )

        # Get the future to await
        future = tracker.get_future(correlation_id)

        # ... backend sends the message ...

        # Resolve when response arrives (from hook or WebSocket)
        tracker.resolve_query(correlation_id, "API is healthy")

        # The awaiter receives the response
        response = await future
    """

    def __init__(self) -> None:
        """Initialize the query tracker."""
        # correlation_id -> PendingQuery
        self._queries: dict[str, PendingQuery] = {}
        # peer_id -> set of correlation_ids (for cancellation on disconnect)
        self._by_peer_id: dict[str, set[str]] = {}

    def register_query(
        self,
        from_peer: str,
        to_peer_id: str,
        to_peer_name: str,
        query_text: str,
        correlation_id: str | None = None,
    ) -> str:
        """Register a new pending query.

        Must be called BEFORE sending the query to ensure the Future exists
        before any response could arrive.

        Args:
            from_peer: Display name of the sending peer
            to_peer_id: peer_id of the target peer
            to_peer_name: Display name of the target peer
            query_text: The query text being sent
            correlation_id: Optional correlation ID (generated if not provided)

        Returns:
            The correlation ID for this query
        """
        if correlation_id is None:
            correlation_id = str(uuid4())

        query = PendingQuery(
            correlation_id=correlation_id,
            from_peer=from_peer,
            to_peer_id=to_peer_id,
            to_peer_name=to_peer_name,
            query_text=query_text,
        )

        self._queries[correlation_id] = query

        # Index by target peer_id for cancellation
        if to_peer_id not in self._by_peer_id:
            self._by_peer_id[to_peer_id] = set()
        self._by_peer_id[to_peer_id].add(correlation_id)

        logger.debug(f"Registered query {correlation_id}: {from_peer} -> {to_peer_name}")
        return correlation_id

    def get_future(self, correlation_id: str) -> asyncio.Future[str] | None:
        """Get the Future for a pending query.

        Args:
            correlation_id: The correlation ID of the query

        Returns:
            The Future that will be resolved with the response, or None if not found
        """
        query = self._queries.get(correlation_id)
        return query.future if query else None

    def resolve_query(self, correlation_id: str, response: str) -> bool:
        """Resolve a pending query with a response.

        Args:
            correlation_id: The correlation ID of the query
            response: The response text

        Returns:
            True if the query was found and resolved, False otherwise
        """
        query = self._queries.get(correlation_id)
        if not query:
            logger.debug(f"No pending query for correlation_id: {correlation_id}")
            return False

        if query.future.done():
            logger.debug(f"Query {correlation_id} already resolved")
            return False

        query.future.set_result(response)
        self._cleanup_query(correlation_id, query)
        logger.debug(f"Resolved query {correlation_id}")
        return True

    def resolve_oldest_query(self, peer_id: str, text: str) -> bool:
        """Resolve the oldest pending query for a peer.

        Used by the HTTP response endpoint when stop hook POSTs a response
        and there's no correlation_id available (pane-based routing).

        Args:
            peer_id: The peer_id of the peer that responded
            text: The response text

        Returns:
            True if a query was resolved, False if no pending queries
        """
        correlation_ids = self._by_peer_id.get(peer_id, set())
        if not correlation_ids:
            return False

        # Find oldest by created_at
        oldest_cid = None
        oldest_time = None
        for cid in correlation_ids:
            query = self._queries.get(cid)
            if query and not query.future.done():
                if oldest_time is None or query.created_at < oldest_time:
                    oldest_time = query.created_at
                    oldest_cid = cid

        if oldest_cid:
            return self.resolve_query(oldest_cid, text)
        return False

    def resolve_query_error(self, correlation_id: str, error: Exception) -> bool:
        """Resolve a pending query with an error.

        Args:
            correlation_id: The correlation ID of the query
            error: The exception to set

        Returns:
            True if the query was found and resolved, False otherwise
        """
        query = self._queries.get(correlation_id)
        if not query:
            logger.debug(f"No pending query for correlation_id: {correlation_id}")
            return False

        if query.future.done():
            logger.debug(f"Query {correlation_id} already resolved")
            return False

        query.future.set_exception(error)
        self._cleanup_query(correlation_id, query)
        logger.debug(f"Resolved query {correlation_id} with error: {error}")
        return True

    def cancel_queries_to_peer(self, peer_id: str) -> int:
        """Cancel all pending queries to a peer.

        Called when a peer goes offline to prevent indefinite waiting.

        Args:
            peer_id: The peer_id of the peer that went offline

        Returns:
            Number of queries cancelled
        """
        correlation_ids = self._by_peer_id.get(peer_id, set()).copy()
        cancelled = 0

        for cid in correlation_ids:
            query = self._queries.get(cid)
            if query and not query.future.done():
                query.future.set_exception(PeerDisconnectedError(query.to_peer_name))
                cancelled += 1
            self._cleanup_query(cid, query)

        logger.info(f"Cancelled {cancelled} queries to peer {peer_id}")
        return cancelled

    def cleanup_query(self, correlation_id: str) -> None:
        """Clean up a query (e.g., after timeout).

        Args:
            correlation_id: The correlation ID of the query
        """
        query = self._queries.get(correlation_id)
        if query:
            self._cleanup_query(correlation_id, query)

    def _cleanup_query(self, correlation_id: str, query: PendingQuery | None) -> None:
        """Internal cleanup of a query from all indexes."""
        self._queries.pop(correlation_id, None)
        if query and query.to_peer_id in self._by_peer_id:
            self._by_peer_id[query.to_peer_id].discard(correlation_id)
            if not self._by_peer_id[query.to_peer_id]:
                del self._by_peer_id[query.to_peer_id]

    def get_pending_count(self) -> int:
        """Get the number of pending queries."""
        return len(self._queries)

    def get_pending_to_peer(self, peer_id: str) -> int:
        """Get the number of pending queries to a specific peer."""
        return len(self._by_peer_id.get(peer_id, set()))
