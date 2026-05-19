"""Tests for the one-shot Scheduler dispatcher."""

from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import cast
from unittest.mock import AsyncMock

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.schedule_store import ScheduleStore
from repowire.daemon.scheduler import Scheduler


def _now_plus(seconds: float) -> datetime:
    return datetime.now(timezone.utc) + timedelta(seconds=seconds)


class _FakeRegistry:
    def __init__(self) -> None:
        self.notify = AsyncMock()
        self.deliver_ask = AsyncMock()


@pytest.fixture
def env(tmp_path: Path):
    store = ScheduleStore(tmp_path / "schedules.json")
    registry = _FakeRegistry()
    at = AskTracker()
    scheduler = Scheduler(
        store=store,
        peer_registry=cast(PeerRegistry, registry),
        ask_tracker=at,
    )
    return scheduler, store, registry, at


async def test_starts_and_stops_cleanly(env) -> None:
    scheduler, _, _, _ = env
    await scheduler.start()
    await asyncio.sleep(0)  # let task park on the wake event
    await scheduler.stop()


async def test_fires_notify_at_due_time(env) -> None:
    scheduler, store, registry, _ = env
    await scheduler.start()
    sched = store.create("alice", "bob", "wake up", _now_plus(0.05), kind="notify")
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    registry.notify.assert_awaited_once()
    assert store.get(sched.schedule_id) is None
    await scheduler.stop()


async def test_fires_ask_and_registers_with_tracker(env) -> None:
    scheduler, store, registry, ask_tracker = env
    await scheduler.start()
    sched = store.create("alice", "bob", "?", _now_plus(0.05), kind="ask")
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    registry.deliver_ask.assert_awaited_once()
    # tracker should have seen the registration (closed on success path is fine)
    assert ask_tracker.total_count() == 1
    assert store.get(sched.schedule_id) is None
    await scheduler.stop()


async def test_delete_before_fire_cancels(env) -> None:
    scheduler, store, registry, _ = env
    await scheduler.start()
    sched = store.create("alice", "bob", "x", _now_plus(0.3))
    scheduler.notify_changed()
    await asyncio.sleep(0.05)
    store.delete(sched.schedule_id)
    scheduler.notify_changed()
    await asyncio.sleep(0.4)
    registry.notify.assert_not_called()
    await scheduler.stop()


async def test_past_due_fires_immediately(env) -> None:
    scheduler, store, registry, _ = env
    await scheduler.start()
    store.create("alice", "bob", "late", _now_plus(-60.0))
    scheduler.notify_changed()
    await asyncio.sleep(0.15)
    registry.notify.assert_awaited_once()
    await scheduler.stop()


async def test_delivery_failure_drops_schedule(env) -> None:
    """One-shot MVP: failures are logged and the schedule is removed."""
    scheduler, store, registry, _ = env
    registry.notify.side_effect = ValueError("no such peer")
    await scheduler.start()
    sched = store.create("alice", "ghost", "x", _now_plus(0.05))
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    assert store.get(sched.schedule_id) is None
    await scheduler.stop()


async def test_recurring_schedule_reschedules_after_fire(env) -> None:
    scheduler, store, registry, _ = env
    await scheduler.start()
    sched = store.create(
        "alice", "alice", "stretch", _now_plus(0.05),
        kind="notify", cron="* * * * *",
    )
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    registry.notify.assert_awaited_once()
    current = store.get(sched.schedule_id)
    assert current is not None
    assert current.cron == "* * * * *"
    assert current.fire_at_dt() > datetime.now(timezone.utc)
    await scheduler.stop()


async def test_earlier_schedule_added_after_later_fires_first(env) -> None:
    scheduler, store, registry, _ = env
    await scheduler.start()
    store.create("a", "b", "later", _now_plus(0.4))
    scheduler.notify_changed()
    await asyncio.sleep(0.05)
    store.create("a", "b", "sooner", _now_plus(0.05))
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    # "sooner" should have fired; "later" still pending
    assert registry.notify.await_count == 1
    args = registry.notify.await_args
    assert args.kwargs["text"] == "sooner"
    await scheduler.stop()


async def test_unexpected_exception_does_not_kill_loop(env) -> None:
    """A delivery that raises something other than ValueError/TransportError
    must not propagate up to _run and kill the scheduler. The bad schedule
    gets dropped; the next one still fires."""
    scheduler, store, registry, _ = env
    # First call raises something the inner except blocks DON'T catch
    # (TypeError stands in for any unexpected exception type).
    registry.notify.side_effect = [TypeError("unexpected"), None]
    await scheduler.start()
    bad = store.create("a", "b", "first-bad", _now_plus(0.05))
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    # Bad schedule got dropped despite the exception.
    assert store.get(bad.schedule_id) is None
    # Scheduler still alive — second schedule fires successfully.
    good = store.create("a", "b", "second-good", _now_plus(0.05))
    scheduler.notify_changed()
    await asyncio.sleep(0.2)
    assert store.get(good.schedule_id) is None
    # Both fires were attempted (and second one succeeded).
    assert registry.notify.await_count == 2
    await scheduler.stop()
