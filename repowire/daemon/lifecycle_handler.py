"""Handles lifecycle events by updating the PeerRegistry.

This module has no knowledge of WHERE events come from (tmux, containers, etc.).
It only reacts to abstract lifecycle events via PeerRegistry methods.
"""

from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

from repowire.hooks._tmux import list_all_panes
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

        Also clears the pane id from the spawn-ownership set so a future tmux
        server restart can't reuse the id and accidentally match an externally
        attached peer.

        Returns number of cancelled queries.
        """
        # Imported here to avoid a routes → handler → routes import cycle.
        from repowire.daemon.routes.spawn import forget_spawned_pane

        forget_spawned_pane(pane_id)

        peer = await self._registry.get_peer_by_pane(pane_id)
        if not peer:
            logger.debug("pane_died: no peer for pane %s", pane_id)
            clear_pane_runtime_state(pane_id)
            return 0

        cancelled = await self._registry.mark_offline(
            peer.peer_id,
            reason="pane_died",
            source="tmux_lifecycle",
            detail="tmux reported that the pane died.",
            context={"pane_id": pane_id},
        )
        await self._transport.disconnect(peer.peer_id)
        clear_pane_runtime_state(pane_id)
        logger.info("pane_died: %s (%s) marked offline", peer.display_name, pane_id)
        return cancelled

    async def handle_session_closed(self, session_name: str) -> int:
        """Offline peers whose pane is genuinely gone after a session close.

        Evidence-gated, not event-trusting. tmux's global ``session-closed``
        hook fires with ``#{session_name}`` resolved against the *surviving*
        attached session, not the one that closed — so when a transient session
        (e.g. a short-lived spawned job) exits, this can arrive naming a circle
        whose agents are all still alive. Blindly offlining + clearing pane
        state on that signal nukes a live circle (and wipes hook metadata,
        breaking inbound reconnect).

        So: probe live tmux panes once. If the named session still has any live
        pane, the event is spurious — do nothing. Otherwise offline only the
        peers whose own pane is no longer present; a peer whose pane is still
        live is kept (pane death has its own ``pane-died`` signal). A tmux probe
        that returns nothing is inconclusive, not "everything died", so we
        refuse to mass-offline on it.
        """
        peers = await self._registry.get_peers_by_circle(session_name)
        if not peers:
            logger.debug("session_closed: no peers in circle %s", session_name)
            return 0

        live_panes = await asyncio.to_thread(list_all_panes)
        if not live_panes:
            # Inconclusive (tmux unreachable / empty listing). Refuse to treat
            # "no evidence" as "all gone" — that's how a probe blip would wipe
            # a live circle. pane-died / lazy repair will catch real deaths.
            logger.warning(
                "session_closed: tmux pane probe returned nothing for %s; "
                "skipping mass-offline (no runtime evidence of closure)",
                session_name,
            )
            return 0

        live_pane_ids = {p["pane_id"] for p in live_panes}
        session_still_live = any(p["session"] == session_name for p in live_panes)
        if session_still_live:
            logger.info(
                "session_closed ignored: session %s still has live panes "
                "(spurious close — likely a transient session exit resolving "
                "#{session_name} to the surviving session)",
                session_name,
            )
            return 0

        # Session is genuinely gone from tmux. Offline only peers whose pane is
        # actually absent; spare any whose pane is somehow still live.
        doomed = [
            p for p in peers if not p.pane_id or p.pane_id not in live_pane_ids
        ]
        spared = len(peers) - len(doomed)
        if not doomed:
            logger.info(
                "session_closed: session %s gone but all %d peers still hold "
                "live panes; nothing offlined",
                session_name,
                len(peers),
            )
            return 0

        async def _offline(peer_id: str) -> int:
            peer = await self._registry.get_peer(peer_id)
            cancelled = await self._registry.mark_offline(
                peer_id,
                reason="session_closed",
                source="tmux_lifecycle",
                detail="tmux session closed and the peer's pane is gone.",
                context={"tmux_session": session_name},
            )
            await self._transport.disconnect(peer_id)
            if peer and peer.pane_id:
                clear_pane_runtime_state(peer.pane_id)
            return cancelled

        results = await asyncio.gather(
            *(_offline(p.peer_id) for p in doomed),
        )

        total = sum(results)
        logger.info(
            "session_closed: marked %d peers offline in circle %s (%d spared "
            "with live panes)",
            len(doomed),
            session_name,
            spared,
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
        """No-op. Window renames must not rewrite peer display_name.

        Why: Renaming would strip the backend suffix (e.g. -claude-code, -codex)
        and create name collisions across backends sharing the same pane group.
        Session registration is the sole source of peer naming.
        """
        logger.debug(
            "window_renamed ignored: session=%s new_name=%s panes=%d",
            session_name, new_name, len(pane_ids),
        )
        return 0

    async def handle_client_detached(self, session_name: str) -> None:
        """Log client detach. No state change for now."""
        logger.info("client_detached: session %s", session_name)
