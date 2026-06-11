"""Shared seed-settle gate.

A peer spawned with a seed message carries ``turn_state == "pending_first_turn"``
until it takes its first turn. During that window the daemon-side spawn seed is
being pasted into the pane composer; any *other* pane injection in the same
window (a queued-notify flush on connect, or a fresh live ask/notify) interleaves
with the seed and mangles both.

``await_seed_settled`` is the single bounded wait both call sites use: hold until
the peer leaves ``pending_first_turn``, or until a timeout, then proceed anyway
(logged). It is intentionally a *wait*, not a re-queue — there is no flush trigger
to re-arm, so re-queueing a live delivery here would strand it.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any

logger = logging.getLogger(__name__)

# How long to hold a pane injection while a spawn seed is in flight before
# giving up and proceeding anyway. Bounded so a seed that never gets picked up
# can't wedge delivery forever — a possible interleave beats a permanent stall.
SEED_SETTLE_WAIT_SECONDS = 25.0
SEED_SETTLE_POLL_SECONDS = 0.5


async def await_seed_settled(
    session_id: str,
    peer_registry: Any,
    *,
    context: str = "pane injection",
) -> Any:
    """Wait while a spawn seed is in flight (peer is ``pending_first_turn``).

    Returns the latest peer snapshot (or None if the peer vanished mid-wait).
    Bounded by ``SEED_SETTLE_WAIT_SECONDS``; on timeout it logs and returns the
    still-pending snapshot so the caller proceeds rather than hanging. ``context``
    only flavors the timeout log line.
    """
    peer = await peer_registry.get_peer(session_id)
    if peer is None or getattr(peer, "turn_state", None) != "pending_first_turn":
        return peer
    deadline = asyncio.get_event_loop().time() + SEED_SETTLE_WAIT_SECONDS
    while getattr(peer, "turn_state", None) == "pending_first_turn":
        if asyncio.get_event_loop().time() >= deadline:
            logger.warning(
                "%s for %s proceeding while still pending_first_turn "
                "(seed not settled within %.0fs)",
                context,
                session_id,
                SEED_SETTLE_WAIT_SECONDS,
            )
            break
        await asyncio.sleep(SEED_SETTLE_POLL_SECONDS)
        peer = await peer_registry.get_peer(session_id)
        if peer is None:
            return None
    return peer
