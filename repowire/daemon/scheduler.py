"""Scheduled check-in dispatcher.

A single background task sleeps until the next-due schedule's `fire_at`, then
fires it. One-shot schedules are removed after firing; recurring cron
schedules are advanced to their next fire time. No polling: the wake event is
only set when the schedule set changes (create/delete) or when something fires.
Past-due schedules at startup fire immediately.

Delivery reuses the existing peer-registry primitives:
  - kind="notify" -> PeerRegistry.notify (fire-and-forget)
  - kind="ask"    -> PeerRegistry.deliver_ask (registers in AskTracker first)

On delivery failure (peer missing or no live WS), one-shot schedules are
dropped and recurring schedules advance to their next cron fire. We log loudly
so misfires are visible.
"""

from __future__ import annotations

import asyncio
import logging
from datetime import datetime, timezone
from typing import TYPE_CHECKING
from uuid import uuid4

from repowire.daemon.schedule_store import Schedule, ScheduleStoreProtocol
from repowire.daemon.websocket_transport import TransportError

if TYPE_CHECKING:
    from repowire.daemon.ask_tracker import AskTracker
    from repowire.daemon.peer_registry import PeerRegistry

logger = logging.getLogger(__name__)

# Cap a single sleep so a long horizon doesn't outlive a clock jump unnoticed.
_MAX_SLEEP_SECONDS = 3600.0


class Scheduler:
    """Drives one-shot scheduled check-ins."""

    def __init__(
        self,
        store: ScheduleStoreProtocol,
        peer_registry: PeerRegistry,
        ask_tracker: AskTracker,
    ) -> None:
        self._store = store
        self._peer_registry = peer_registry
        self._ask_tracker = ask_tracker
        self._wake = asyncio.Event()
        self._task: asyncio.Task[None] | None = None
        self._stopped = False

    async def start(self) -> None:
        if self._task is not None:
            return
        self._stopped = False
        self._task = asyncio.create_task(self._run(), name="scheduler")

    async def stop(self) -> None:
        self._stopped = True
        self._wake.set()
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):
                pass
            self._task = None
        self._store.persist()

    def notify_changed(self) -> None:
        """Signal the loop that the schedule set changed (create/delete)."""
        self._wake.set()

    async def _run(self) -> None:
        while not self._stopped:
            self._wake.clear()
            nxt = self._store.next_due()
            if nxt is None:
                await self._wake.wait()
                continue
            now = datetime.now(timezone.utc)
            delay = (nxt.fire_at_dt() - now).total_seconds()
            if delay > 0:
                try:
                    await asyncio.wait_for(
                        self._wake.wait(),
                        timeout=min(delay, _MAX_SLEEP_SECONDS),
                    )
                    # Woken by a change — reloop and recompute next due.
                    continue
                except asyncio.TimeoutError:
                    pass
            # Re-check that this schedule still exists (delete may have raced).
            current = self._store.get(nxt.schedule_id)
            if current is None:
                continue
            await self._fire(current)

    async def _fire(self, sched: Schedule) -> None:
        try:
            if sched.kind == "ask":
                cid = f"ask-{uuid4().hex[:8]}"
                await self._ask_tracker.register(
                    from_peer_id=sched.from_peer,
                    from_peer_name=sched.from_peer,
                    to_peer_id=sched.to_peer,
                    to_peer_name=sched.to_peer,
                    text=sched.text,
                    correlation_id=cid,
                )
                try:
                    await self._peer_registry.deliver_ask(
                        from_peer=sched.from_peer,
                        to_peer=sched.to_peer,
                        text=sched.text,
                        correlation_id=cid,
                        circle=sched.circle,
                    )
                except (ValueError, TransportError) as e:
                    await self._ask_tracker.close(cid, reason="send_failed")
                    logger.warning(
                        "Scheduled ask %s failed to deliver: %s",
                        sched.schedule_id, e,
                    )
            else:
                try:
                    await self._peer_registry.notify(
                        from_peer=sched.from_peer,
                        to_peer=sched.to_peer,
                        text=sched.text,
                        circle=sched.circle,
                    )
                except (ValueError, TransportError) as e:
                    logger.warning(
                        "Scheduled notify %s failed to deliver: %s",
                        sched.schedule_id, e,
                    )
            logger.info(
                "Fired schedule %s: %s -> %s (%s)",
                sched.schedule_id, sched.from_peer, sched.to_peer, sched.kind,
            )
        except Exception as e:
            # Catch-all so a misbehaving delivery (anything beyond the inner
            # ValueError/TransportError handlers above) doesn't escape and
            # kill the scheduler task. The loop continues to the next
            # schedule; this one still gets dropped via the finally block.
            logger.exception(
                "Scheduled %s %s: unexpected error during fire (%s); dropping",
                sched.kind, sched.schedule_id, e,
            )
        finally:
            if sched.cron:
                try:
                    self._store.reschedule_next(sched.schedule_id)
                except ValueError as e:
                    logger.warning(
                        "Recurring schedule %s has invalid cron after fire (%s); dropping",
                        sched.schedule_id, e,
                    )
                    self._store.delete(sched.schedule_id)
            else:
                self._store.delete(sched.schedule_id)
            self._store.persist()
