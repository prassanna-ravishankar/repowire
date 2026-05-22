"""In-process pub/sub for peer lifecycle events.

Scope: typed publisher/subscriber for peer status/liveness transitions.
No persistence, no external transport — handlers run in the daemon process.

The existing PeerRegistry event surface is unchanged; this bus is additive.
Subscribers are isolated: one bad handler does not
break delivery to others, and publish never raises.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class PeerEvent:
    """Base type for peer lifecycle events."""


@dataclass(frozen=True)
class PeerStatusChanged(PeerEvent):
    """Emitted when a peer transitions between PeerStatus values."""

    peer_id: str
    display_name: str
    old_status: PeerStatus
    new_status: PeerStatus


Handler = Callable[[PeerEvent], Awaitable[None]]


class EventBus:
    """Minimal async pub/sub. Fire-and-forget delivery, isolated failures."""

    def __init__(self) -> None:
        self._subscribers: dict[int, Handler] = {}
        self._next_token = 0

    def subscribe(self, handler: Handler) -> int:
        token = self._next_token
        self._next_token += 1
        self._subscribers[token] = handler
        return token

    def unsubscribe(self, token: int) -> None:
        self._subscribers.pop(token, None)

    def publish(self, event: PeerEvent) -> None:
        for handler in list(self._subscribers.values()):
            asyncio.create_task(self._safe_deliver(handler, event))

    async def _safe_deliver(self, handler: Handler, event: PeerEvent) -> None:
        try:
            await handler(event)
        except Exception:
            logger.exception("EventBus subscriber raised; isolating")
