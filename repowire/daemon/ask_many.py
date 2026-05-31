"""In-memory ask-many fanout tracking (repowire-sip.8).

A parent ``askm-`` anchor fans out to N normal child asks (each a regular
``ask-`` row carrying ``parent_id``). The parent is never delivered and never
appears in ``/asks/pending``; children deliver + ack exactly like any ask.
Aggregation derives the parent view from the children at read time.

v1 is deliberately dumb: no quorum, no retry, no map-reduce, no background
timeouts. Timeout/partial state is computed lazily at GET time from the parent
deadline. In-memory only, like AskTracker — parent + children vanish on daemon
restart.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any
from uuid import uuid4

from repowire.daemon.ask_tracker import Ask, AskTracker

DEFAULT_ASK_MANY_TIMEOUT_SECONDS = 300
MAX_ASK_MANY_PEERS = 50


def new_ask_many_id() -> str:
    return f"askm-{uuid4().hex[:8]}"


@dataclass
class AskManyChild:
    """A child entry: a resolved peer + its correlation_id, or a pre-registration
    failure (no correlation_id) when the peer couldn't be resolved/delivered."""

    peer_name: str
    correlation_id: str | None = None
    peer_id: str | None = None
    delivery_error: str | None = None


@dataclass
class AskManyParent:
    parent_id: str
    from_peer_id: str | None
    from_peer_name: str
    text: str
    children: list[AskManyChild] = field(default_factory=list)
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    timeout_seconds: int = DEFAULT_ASK_MANY_TIMEOUT_SECONDS


def _child_status(ask: Ask | None) -> str:
    """Derive a child's status from its (optional) Ask row.

    failed: never registered (resolution/delivery failure) or closed send_failed.
    replied: closed with a captured reply body.
    acked: closed without a body (bare ack) or via reply_to.
    pending: still open.
    """
    if ask is None:
        return "failed"
    if not ask.closed:
        return "pending"
    if ask.close_reason == "send_failed":
        return "failed"
    if ask.reply_text is not None:
        return "replied"
    return "acked"


class AskManyTracker:
    """Holds ask-many parents in memory; children live in the AskTracker."""

    def __init__(self, ask_tracker: AskTracker) -> None:
        self._ask_tracker = ask_tracker
        self._parents: dict[str, AskManyParent] = {}
        self._lock = asyncio.Lock()

    async def create(
        self,
        *,
        from_peer_id: str | None,
        from_peer_name: str,
        text: str,
        timeout_seconds: int = DEFAULT_ASK_MANY_TIMEOUT_SECONDS,
    ) -> AskManyParent:
        parent = AskManyParent(
            parent_id=new_ask_many_id(),
            from_peer_id=from_peer_id,
            from_peer_name=from_peer_name,
            text=text,
            timeout_seconds=timeout_seconds,
        )
        async with self._lock:
            self._parents[parent.parent_id] = parent
        return parent

    async def add_child(self, parent_id: str, child: AskManyChild) -> None:
        async with self._lock:
            parent = self._parents.get(parent_id)
            if parent is not None:
                parent.children.append(child)

    async def get(self, parent_id: str) -> AskManyParent | None:
        return self._parents.get(parent_id)

    async def status(self, parent_id: str, *, now: datetime | None = None) -> dict[str, Any] | None:
        """Aggregate the current parent view from its children. Lazy timeout."""
        parent = self._parents.get(parent_id)
        if parent is None:
            return None
        now = now or datetime.now(timezone.utc)

        child_views: list[dict[str, Any]] = []
        counts = {"acked": 0, "replied": 0, "pending": 0, "failed": 0}
        for child in parent.children:
            ask = (
                await self._ask_tracker.get(child.correlation_id)
                if child.correlation_id
                else None
            )
            status = _child_status(ask)
            counts[status] += 1
            child_views.append(
                {
                    "peer": child.peer_name,
                    "peer_id": child.peer_id,
                    "correlation_id": child.correlation_id,
                    "status": status,
                    "reply": ask.reply_text if ask else None,
                    "close_reason": ask.close_reason if ask else None,
                    "error": child.delivery_error,
                }
            )

        deadline = parent.created_at + timedelta(seconds=parent.timeout_seconds)
        any_open = counts["pending"] > 0
        timed_out = any_open and now >= deadline
        if not any_open:
            state = "complete"
        elif timed_out:
            state = "partial"
        else:
            state = "pending"

        return {
            "parent_id": parent.parent_id,
            "from_peer": parent.from_peer_name,
            "text": parent.text,
            "created_at": parent.created_at.isoformat(),
            "deadline": deadline.isoformat(),
            "state": state,
            "timed_out": timed_out,
            "rollup": {"total": len(parent.children), **counts},
            "children": child_views,
        }
