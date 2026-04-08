"""Handles lifecycle events by updating the PeerRegistry.

This module has no knowledge of WHERE events come from (tmux, containers, etc.).
It only reacts to abstract lifecycle events via PeerRegistry methods.
"""

from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from repowire.daemon.peer_registry import PeerRegistry
    from repowire.daemon.query_tracker import QueryTracker
    from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)


class LifecycleHandler:
    """Reacts to lifecycle events by mutating peer state."""

    def __init__(
        self,
        peer_registry: PeerRegistry,
        query_tracker: QueryTracker,
        transport: WebSocketTransport,
    ) -> None:
        self._registry = peer_registry
        self._tracker = query_tracker
        self._transport = transport

    async def handle_pane_died(self, pane_id: str) -> int:
        """Mark the peer in this pane OFFLINE and disconnect its transport.

        Returns number of cancelled queries.
        """
        peer = await self._registry.get_peer_by_pane(pane_id)
        if not peer:
            logger.debug("pane_died: no peer for pane %s", pane_id)
            return 0

        cancelled = await self._registry.mark_offline(peer.peer_id)
        await self._transport.disconnect(peer.peer_id)
        logger.info("pane_died: %s (%s) marked offline", peer.display_name, pane_id)
        return cancelled

    async def handle_session_closed(self, session_name: str) -> int:
        """Batch-offline all peers in the given circle (session).

        Returns total cancelled queries.
        """
        peers = await self._registry.get_peers_by_circle(session_name)
        if not peers:
            logger.debug("session_closed: no peers in circle %s", session_name)
            return 0

        async def _offline(peer_id: str) -> int:
            cancelled = await self._registry.mark_offline(peer_id)
            await self._transport.disconnect(peer_id)
            return cancelled

        results = await asyncio.gather(
            *(_offline(p.peer_id) for p in peers),
        )

        total = sum(results)
        logger.info(
            "session_closed: marked %d peers offline in circle %s",
            len(peers),
            session_name,
        )
        return total

    async def handle_session_renamed(
        self, old_name: str, new_name: str,
    ) -> int:
        """Update circle name for all peers in the old circle.

        Returns number of peers updated.
        """
        peers = await self._registry.get_peers_by_circle(old_name)
        if not peers:
            logger.debug("session_renamed: no peers in circle %s", old_name)
            return 0

        for peer in peers:
            await self._registry.set_peer_circle(peer.peer_id, new_name)

        logger.info(
            "session_renamed: moved %d peers from %s → %s",
            len(peers),
            old_name,
            new_name,
        )
        return len(peers)

    async def handle_window_renamed(
        self, session_name: str, old_name: str, new_name: str,
    ) -> bool:
        """Update display name for the peer matching circle + old window name.

        Returns True if a peer was updated.
        """
        peers = await self._registry.get_peers_by_circle(session_name)
        for peer in peers:
            if peer.display_name == old_name:
                ok = await self._registry.update_peer_display_name(
                    peer.peer_id, new_name,
                )
                if ok:
                    logger.info(
                        "window_renamed: %s → %s in circle %s",
                        old_name,
                        new_name,
                        session_name,
                    )
                return ok

        logger.debug(
            "window_renamed: no peer named %s in circle %s",
            old_name,
            session_name,
        )
        return False

    async def handle_client_detached(self, session_name: str) -> None:
        """Log client detach. No state change for now."""
        logger.info("client_detached: session %s", session_name)
