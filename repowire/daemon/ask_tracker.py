"""Ask/ack lifecycle tracking for the Repowire daemon.

The AskTracker manages the non-blocking ask/ack lifecycle introduced in the
ask-ack-notify refactor. Asks are registered when a peer fires `ask()`, the
target peer picks them up on its next Stop hook (turn boundary), and they
are closed when the target either:

  - calls `ack(corr_id)` — bare close, no content
  - calls `ack(corr_id, msg)` — close with reply content delivered to asker
  - calls `ask(reply_to=corr_id, ...)` — opens a new thread + closes this one

If a peer picks up an ask but doesn't ack/reply within one full turn after
pickup, the Stop hook injects a reminder (capped to 3 most recent, once-only).

Unlike QueryTracker (which used asyncio Futures for synchronous blocking),
AskTracker is purely a state store. Reply delivery is handled by the
notification pipeline (so it inherits BUSY-buffering for free).

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


@dataclass
class Ask:
    """An open ask awaiting ack/reply.

    picked_up_turn_seq: per-peer turn counter at pickup. Compared against the
        next pending-poll's seq for the one-turn grace check. None until pickup.
    close_reason: 'ack' | 'ack_with_msg' | 'reply_to' | 'evicted'.
    """

    correlation_id: str
    from_peer_id: str
    from_peer_name: str
    to_peer_id: str
    to_peer_name: str
    text: str
    reply_to: str | None = None
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    picked_up: bool = False
    picked_up_at: datetime | None = None
    picked_up_turn_seq: int | None = None
    reminded: bool = False
    closed: bool = False
    close_reason: str | None = None


class AskTracker:
    """In-memory store for the ask/ack lifecycle.

    All mutating methods are async-locked. Read methods are unlocked snapshots
    (callers that need consistency should re-check after acting).
    """

    def __init__(self, *, ttl_hours: float = 24.0) -> None:
        self._lock = asyncio.Lock()
        self._asks: dict[str, Ask] = {}
        self._turn_seq: dict[str, int] = {}
        self._ttl = timedelta(hours=ttl_hours)
        self._last_eviction: float = 0.0

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
        """
        async with self._lock:
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

    async def mark_picked_up(self, correlation_id: str) -> bool:
        """Mark an ask as picked up by the recipient.

        Snapshots the recipient's current turn_seq atomically (under the same
        lock that increment_turn uses). Clients don't supply turn_seq — they
        only signal "delivered." This keeps the grace-window math daemon-owned.

        Idempotent — first call sticks; later calls are no-ops so replays
        can't shift the grace window.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask or ask.closed or ask.picked_up:
                return False
            ask.picked_up = True
            ask.picked_up_at = datetime.now(timezone.utc)
            ask.picked_up_turn_seq = self._turn_seq.get(ask.to_peer_id, 0)
            return True

    async def mark_reminded(self, correlation_id: str) -> bool:
        """Flip reminded=True. Once-only."""
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask or ask.closed or ask.reminded:
                return False
            ask.reminded = True
            return True

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

    async def increment_turn(self, peer_id: str) -> int:
        """Bump the per-peer turn counter and return the new value.

        Called from the Stop hook intake path. Each Stop fire for a pane is
        one turn. The counter is used to compare against picked_up_turn_seq
        for the grace-window check.
        """
        async with self._lock:
            self._turn_seq[peer_id] = self._turn_seq.get(peer_id, 0) + 1
            return self._turn_seq[peer_id]

    async def pending_for_peer(
        self,
        peer_id: str,
        current_turn_seq: int,
        max_results: int = 3,
    ) -> list[Ask]:
        """Return open asks targeting this peer that are due for a reminder.

        Selects asks where:
          - to_peer_id == peer_id
          - picked_up == True
          - reminded == False
          - closed == False
          - picked_up_turn_seq < current_turn_seq  (one full turn of grace)

        Returns the most recent `max_results` asks, newest first.

        Lazy-repair: opportunistically evicts TTL-expired asks at most once
        per _EVICTION_INTERVAL_SECONDS. Stop hooks call this on every turn,
        so the dict gets swept regularly without a background timer.
        """
        await self._maybe_evict_expired()
        async with self._lock:
            candidates = [
                ask for ask in self._asks.values()
                if ask.to_peer_id == peer_id
                and ask.picked_up
                and not ask.reminded
                and not ask.closed
                and ask.picked_up_turn_seq is not None
                and ask.picked_up_turn_seq < current_turn_seq
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
        """Drop turn counter and any asks involving this peer.

        Called by PeerRegistry when pruning offline peers, so the tracker's
        memory footprint is bounded by the live peer set.

        Returns the number of asks dropped.
        """
        async with self._lock:
            self._turn_seq.pop(peer_id, None)
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
