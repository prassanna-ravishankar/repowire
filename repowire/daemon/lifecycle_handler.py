"""Handles lifecycle events by updating the PeerRegistry.

This module has no knowledge of WHERE events come from (tmux, containers, etc.).
It only reacts to abstract lifecycle events via PeerRegistry methods.
"""

from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

from repowire.hooks.utils import clear_pane_runtime_state

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
            clear_pane_runtime_state(pane_id)
            return 0

        cancelled = await self._registry.mark_offline(peer.peer_id)
        await self._transport.disconnect(peer.peer_id)
        clear_pane_runtime_state(pane_id)
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
            peer = await self._registry.get_peer(peer_id)
            cancelled = await self._registry.mark_offline(peer_id)
            await self._transport.disconnect(peer_id)
            if peer and peer.pane_id:
                clear_pane_runtime_state(peer.pane_id)
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
        self, new_name: str, pane_ids: list[str],
    ) -> int:
        """Update circle for peers identified by their pane IDs.

        Returns number of peers updated.
        """
        async def _rename(pane_id: str) -> bool:
            peer = await self._registry.get_peer_by_pane(pane_id)
            if peer and peer.circle != new_name:
                await self._registry.set_peer_circle(peer.peer_id, new_name)
                return True
            return False

        results = await asyncio.gather(*(_rename(pid) for pid in pane_ids))
        count = sum(results)
        if count:
            logger.info("session_renamed: moved %d peers → %s", count, new_name)
        return count

    async def handle_window_renamed(
        self, session_name: str, new_name: str, pane_ids: list[str],
    ) -> int:
        """Update display name for peers identified by their pane IDs.

        Returns number of peers updated.
        """
        async def _rename(pane_id: str) -> bool:
            peer = await self._registry.get_peer_by_pane(pane_id)
            if peer and peer.display_name != new_name:
                ok = await self._registry.update_peer_display_name(
                    peer.peer_id, new_name,
                )
                if ok:
                    logger.info(
                        "window_renamed: %s → %s in circle %s",
                        peer.display_name, new_name, session_name,
                    )
                return ok
            return False

        results = await asyncio.gather(*(_rename(pid) for pid in pane_ids))
        return sum(results)

    async def handle_client_detached(self, session_name: str) -> None:
        """Log client detach. No state change for now."""
        logger.info("client_detached: session %s", session_name)
