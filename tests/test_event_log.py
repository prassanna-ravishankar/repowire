from __future__ import annotations

import asyncio
import json

import pytest

from repowire.daemon.app import create_test_app
from repowire.daemon.event_log import EventLog


def test_add_event_assigns_id_timestamp_and_marks_dirty(tmp_path):
    log = EventLog(tmp_path / "events.json")

    event_id = log.add_event("chat_turn", {"peer": "alice", "text": "hello"})

    assert log.dirty is True
    assert len(log.events) == 1
    event = log.events[0]
    assert event["id"] == event_id
    assert event["type"] == "chat_turn"
    assert "timestamp" in event
    assert event["peer"] == "alice"
    assert event["text"] == "hello"


def test_save_is_debounced_by_dirty_flag(tmp_path):
    path = tmp_path / "events.json"
    log = EventLog(path)

    log.save()
    assert not path.exists()

    log.add_event("notification", {"text": "one"})
    log.save()

    assert log.dirty is False
    assert json.loads(path.read_text())[0]["type"] == "notification"


def test_load_persists_last_100_events(tmp_path):
    path = tmp_path / "events.json"
    path.write_text(json.dumps([
        {"id": f"event-{i}", "type": "test", "timestamp": "t"}
        for i in range(120)
    ]))

    log = EventLog(path)

    assert len(log.events) == 100
    assert log.events[0]["id"] == "event-20"
    assert log.events[-1]["id"] == "event-119"
    assert log.dirty is False


def test_events_since_and_update_event_mark_dirty(tmp_path):
    log = EventLog(tmp_path / "events.json")
    first = log.add_event("first", {})
    second = log.add_event("second", {})
    third = log.add_event("third", {})
    log.dirty = False

    assert [event["id"] for event in log.events_since(first)] == [second, third]
    assert log.events_since(third) == []
    assert [event["id"] for event in log.events_since("missing")] == [
        first, second, third,
    ]

    assert log.update_event(second, {"status": "done"}) is True
    assert log.dirty is True
    assert log.events[1]["status"] == "done"
    assert log.update_event("missing", {"status": "lost"}) is False


@pytest.mark.asyncio
async def test_subscribers_are_woken_and_can_unsubscribe(tmp_path):
    log = EventLog(tmp_path / "events.json")
    first = log.subscribe()
    second = log.subscribe()

    log.add_event("wake", {})
    await asyncio.wait_for(asyncio.gather(first.wait(), second.wait()), timeout=0.5)

    log.unsubscribe(first)
    first.clear()
    second.clear()
    log.add_event("wake-again", {})

    assert not first.is_set()
    assert second.is_set()


@pytest.mark.asyncio
async def test_create_test_app_persists_events_on_shutdown(tmp_path):
    app = create_test_app(persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        app.state.event_log.add_event("shutdown", {"text": "persist me"})

    data = json.loads((tmp_path / "events.json").read_text())
    assert len(data) == 1
    assert data[0]["type"] == "shutdown"
