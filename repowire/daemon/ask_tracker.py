"""Ask/ack lifecycle tracking for the Repowire daemon.

The AskTracker is the source of truth for open ask threads. Asks are
registered when a peer fires `ask()`, the recipient transport injects the
wire frame on receipt, and the ask is closed when the recipient either:

  - calls `ack(corr_id)` — bare close, no content
  - calls `ack(corr_id, msg)` — close with reply content delivered to asker
  - calls `ask(reply_to=corr_id, ...)` — opens a new thread + closes this one

Open asks targeting a peer are surfaced on every Stop hook poll via
`/asks/pending`, so an agent that hasn't acked will be reminded on every
subsequent turn until they do. Reply delivery is handled by the
notification pipeline (framed `[ack #cid from @peer] msg`).

In-memory only. Asks are evicted after 24h via TTL sweep mirroring the
config's prune_max_age_hours.
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from uuid import uuid4

logger = logging.getLogger(__name__)

_EVICTION_INTERVAL_SECONDS = 300.0


class QuiesceFailedError(Exception):
    """Raised when begin_quiesce cannot acquire the barrier because open
    asks already exist for the peer."""

    def __init__(self, peer_id: str, open_cids: list[str]) -> None:
        super().__init__(f"Peer {peer_id} has open asks: {open_cids}")
        self.peer_id = peer_id
        self.open_cids = open_cids


class QuiescedError(Exception):
    """Raised when register() is called while a peer is mid-switch."""

    def __init__(self, peer_id: str) -> None:
        super().__init__(f"Peer {peer_id} is mid-switch; ask refused")
        self.peer_id = peer_id


@dataclass
class Ask:
    """An open ask awaiting ack/reply.

    close_reason: 'ack' | 'ack_with_msg' | 'reply_to' | 'evicted' | 'send_failed'.

    ``pending_reply`` is the durable home for a completed reply whose first
    delivery attempt failed because the asker had no live transport. The
    ACP-routed `/ask` path uses this to retain a successful ACP answer across
    asker reconnects — the WS path doesn't need it because /ack's MCP caller
    handles retry on 503. Cleared on successful redelivery (which also closes
    the ask).
    """

    correlation_id: str
    from_peer_id: str
    from_peer_name: str
    to_peer_id: str
    to_peer_name: str
    text: str
    reply_to: str | None = None
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    closed: bool = False
    close_reason: str | None = None
    pending_reply: str | None = None


class AskTracker:
    """In-memory store of open ask threads.

    All mutating methods are async-locked. Read methods are unlocked snapshots
    (callers that need consistency should re-check after acting).
    """

    def __init__(self, *, ttl_hours: float = 24.0) -> None:
        self._lock = asyncio.Lock()
        self._asks: dict[str, Ask] = {}
        self._ttl = timedelta(hours=ttl_hours)
        self._last_eviction: float = 0.0
        # peer_ids currently mid-switch — new asks to/from them are refused so
        # the switch route can kill+respawn without orphaning correlation IDs.
        self._quiescing: set[str] = set()


    async def begin_quiesce(self, peer_id: str) -> None:
        """Atomically: verify no open asks for peer, mark peer as quiescing.

        While quiesced, register() rejects new asks for this peer in either
        direction. Use end_quiesce() in a finally to release.

        Exclusive: a concurrent begin_quiesce for the same peer_id raises
        QuiesceFailedError with an empty open_cids list, so only one caller
        ever holds the barrier at a time. Without this, two concurrent
        switches against the same peer would both enter the critical
        kill/spawn section and the first end_quiesce would prematurely
        release the barrier for the second.

        Raises QuiesceFailedError if open asks exist OR the peer is already
        quiescing.
        """
        async with self._lock:
            if peer_id in self._quiescing:
                raise QuiesceFailedError(peer_id, [])
            open_asks = [
                a.correlation_id for a in self._asks.values()
                if not a.closed and (a.to_peer_id == peer_id or a.from_peer_id == peer_id)
            ]
            if open_asks:
                raise QuiesceFailedError(peer_id, open_asks)
            self._quiescing.add(peer_id)
            logger.debug("Quiesced peer %s for switch", peer_id)

    async def end_quiesce(self, peer_id: str) -> None:
        """Release the switch barrier. Idempotent."""
        async with self._lock:
            self._quiescing.discard(peer_id)
            logger.debug("Released quiesce for peer %s", peer_id)

    async def register(
        self,
        from_peer_id: str,
        from_peer_name: str,
        to_peer_id: str,
        to_peer_name: str,
        text: str,
        reply_to: str | None = None,
        correlation_id: str | None = None,
    ) -> str:
        """Register a new open ask. Returns the correlation_id.

        If correlation_id is supplied and already exists, this is treated as
        a retry: the existing entry is preserved (lifecycle state intact)
        and the same id is returned. Auto-generated cids never collide.

        Raises QuiescedError if either endpoint is mid-switch.
        """
        async with self._lock:
            if to_peer_id in self._quiescing:
                raise QuiescedError(to_peer_id)
            if from_peer_id in self._quiescing:
                raise QuiescedError(from_peer_id)
            cid = correlation_id or f"ask-{uuid4().hex[:8]}"
            if cid in self._asks:
                logger.debug("Ask %s already registered; treating as retry", cid)
                return cid
            self._asks[cid] = Ask(
                correlation_id=cid,
                from_peer_id=from_peer_id,
                from_peer_name=from_peer_name,
                to_peer_id=to_peer_id,
                to_peer_name=to_peer_name,
                text=text,
                reply_to=reply_to,
            )
            logger.debug("Registered ask %s: %s -> %s", cid, from_peer_name, to_peer_name)
            return cid

    async def close(self, correlation_id: str, reason: str) -> Ask | None:
        """Close an ask. Returns the Ask if it existed and wasn't already closed."""
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask or ask.closed:
                return None
            ask.closed = True
            ask.close_reason = reason
            logger.debug("Closed ask %s: %s", correlation_id, reason)
            return ask

    async def get(self, correlation_id: str) -> Ask | None:
        """Look up an ask by corr_id."""
        return self._asks.get(correlation_id)

    async def set_pending_reply(self, correlation_id: str, framed_reply: str) -> bool:
        """Stash a completed reply on an open ask for later redelivery.

        Used by the ACP-routed `/ask` path when notify fails with
        TransportError: the assembled ACP answer is real and must not be
        dropped just because the asker is currently offline. Returns True
        if the ask exists and was still open, False otherwise.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask or ask.closed:
                return False
            ask.pending_reply = framed_reply
            logger.debug("Stashed pending reply on ask %s", correlation_id)
            return True

    async def clear_pending_reply(self, correlation_id: str) -> None:
        """Drop the stashed reply from an ask (idempotent).

        Used after successful redelivery so the closed Ask object doesn't
        retain reply text it can never use again.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is not None:
                ask.pending_reply = None

    async def take_pending_replies_for_asker(self, peer_id: str) -> list[Ask]:
        """Snapshot open asks targeting this asker that have a stashed reply.

        Caller (peer_registry reconnect path) attempts redelivery for each;
        on success it closes the ask via ``close`` and the reply will not
        be re-driven. We snapshot rather than drain so a failed redelivery
        leaves the reply in place for the next reconnect.
        """
        async with self._lock:
            return [
                ask for ask in self._asks.values()
                if ask.from_peer_id == peer_id
                and not ask.closed
                and ask.pending_reply is not None
            ]

    async def pending_for_peer(
        self,
        peer_id: str,
        max_results: int = 10,
        direction: str = "inbound",
    ) -> list[Ask]:
        """Return open asks for this peer, newest first, capped at max_results.

        direction:
          - "inbound"  (default): asks targeting this peer (to_peer_id == peer_id)
          - "outbound": asks this peer opened (from_peer_id == peer_id)
          - "both":    union of the two

        Lazy-repair: opportunistically evicts TTL-expired asks at most once
        per _EVICTION_INTERVAL_SECONDS. Stop hooks call this on each response,
        so the dict gets swept regularly without a background timer.
        """
        if direction not in ("inbound", "outbound", "both"):
            raise ValueError(f"invalid direction: {direction!r}")
        await self._maybe_evict_expired()
        async with self._lock:
            def matches(ask: Ask) -> bool:
                if direction == "inbound":
                    return ask.to_peer_id == peer_id
                if direction == "outbound":
                    return ask.from_peer_id == peer_id
                return ask.to_peer_id == peer_id or ask.from_peer_id == peer_id

            candidates = [
                ask for ask in self._asks.values()
                if matches(ask) and not ask.closed
            ]
            candidates.sort(key=lambda a: a.created_at, reverse=True)
            return candidates[:max_results]

    async def _maybe_evict_expired(self) -> None:
        """Run TTL eviction if enough wall time has passed since the last sweep."""
        now = time.monotonic()
        if now - self._last_eviction < _EVICTION_INTERVAL_SECONDS:
            return
        self._last_eviction = now
        await self.evict_expired()

    async def forget_peer(self, peer_id: str) -> int:
        """Drop any asks involving this peer.

        Called by PeerRegistry when pruning offline peers, so the tracker's
        memory footprint is bounded by the live peer set.

        Returns the number of asks dropped.
        """
        async with self._lock:
            doomed = [
                cid for cid, ask in self._asks.items()
                if ask.to_peer_id == peer_id or ask.from_peer_id == peer_id
            ]
            for cid in doomed:
                ask = self._asks[cid]
                if not ask.closed:
                    ask.closed = True
                    ask.close_reason = "evicted"
                del self._asks[cid]
            return len(doomed)

    async def evict_expired(self) -> int:
        """Drop asks older than TTL. Returns count evicted.

        Closes them as 'evicted' before removal so any caller holding a
        reference can see why they vanished.
        """
        cutoff = datetime.now(timezone.utc) - self._ttl
        async with self._lock:
            expired = [
                cid for cid, ask in self._asks.items()
                if ask.created_at < cutoff
            ]
            for cid in expired:
                ask = self._asks[cid]
                if not ask.closed:
                    ask.closed = True
                    ask.close_reason = "evicted"
                del self._asks[cid]
            if expired:
                logger.info("Evicted %d expired asks", len(expired))
            return len(expired)

    def open_count(self) -> int:
        """Number of open (non-closed) asks. For diagnostics."""
        return sum(1 for ask in self._asks.values() if not ask.closed)

    def total_count(self) -> int:
        """Total asks tracked (open + closed-but-retained). For diagnostics."""
        return len(self._asks)
