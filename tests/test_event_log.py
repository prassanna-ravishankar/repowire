from __future__ import annotations

import asyncio
import json

import pytest

from repowire.config.models import Config
from repowire.daemon.app import create_test_app
from repowire.daemon.event_log import EventLog
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.events import SQLiteEventStore


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
async def test_create_test_app_uses_sqlite_even_when_legacy_flag_is_false(tmp_path):
    cfg = Config(experiments={"sqlite_state": False})
    app = create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        app.state.event_log.add_event("shutdown", {"text": "persist me"})

    assert not (tmp_path / "events.json").exists()
    db = StateDatabase(tmp_path / "state.db")
    try:
        row = db.conn.execute("SELECT payload_json FROM events").fetchone()
        assert row is not None
        event = json.loads(row["payload_json"])
        assert event["type"] == "shutdown"
        assert event["text"] == "persist me"
    finally:
        db.close()


def test_sqlite_event_store_imports_legacy_events_once(tmp_path):
    legacy_path = tmp_path / "events.json"
    legacy_path.write_text(json.dumps([
        {
            "id": "event-1",
            "type": "chat_turn",
            "timestamp": "2026-05-22T09:00:00+00:00",
            "peer": "alice",
            "text": "hello",
        },
    ]))

    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteEventStore(db, legacy_path=legacy_path)
        assert store.count() == 1
        assert store.load_recent(500)[0]["text"] == "hello"
        row = db.conn.execute(
            "SELECT row_count, status FROM legacy_imports WHERE source_path = ?",
            (str(legacy_path),),
        ).fetchone()
        assert row["row_count"] == 1
        assert row["status"] == "ok"

        store.append({
            "id": "event-2",
            "type": "notification",
            "timestamp": "2026-05-22T09:01:00+00:00",
        })
        SQLiteEventStore(db, legacy_path=legacy_path)
        assert store.count() == 2
    finally:
        db.close()


def test_sqlite_event_store_import_guard_uses_legacy_import_audit(tmp_path):
    legacy_path = tmp_path / "events.json"
    legacy_path.write_text(json.dumps([
        {
            "id": "legacy-event",
            "type": "chat_turn",
            "timestamp": "2026-05-22T09:00:00+00:00",
        },
    ]))

    db = StateDatabase(tmp_path / "state.db")
    try:
        existing_store = SQLiteEventStore(db)
        existing_store.append({
            "id": "sqlite-event",
            "type": "notification",
            "timestamp": "2026-05-22T09:01:00+00:00",
        })

        imported_store = SQLiteEventStore(db, legacy_path=legacy_path)
        assert imported_store.count() == 2
        assert {event["id"] for event in imported_store.load_recent(10)} == {
            "legacy-event",
            "sqlite-event",
        }
        row = db.conn.execute(
            "SELECT row_count, status FROM legacy_imports WHERE source_path = ?",
            (str(legacy_path),),
        ).fetchone()
        assert row["row_count"] == 1
        assert row["status"] == "ok"
    finally:
        db.close()


def test_sqlite_event_store_records_corrupt_legacy_import(tmp_path):
    legacy_path = tmp_path / "events.json"
    legacy_path.write_text("{not json")

    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteEventStore(db, legacy_path=legacy_path)
        assert store.count() == 0
        row = db.conn.execute(
            "SELECT row_count, status, error FROM legacy_imports WHERE source_path = ?",
            (str(legacy_path),),
        ).fetchone()
        assert row["row_count"] == 0
        assert row["status"] == "error"
        assert row["error"] == "JSONDecodeError"
    finally:
        db.close()


def test_sqlite_event_store_load_recent_preserves_equal_timestamp_insert_order(tmp_path):
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteEventStore(db)
        timestamp = "2026-05-22T09:00:00+00:00"
        for event_id in ("event-1", "event-2", "event-3"):
            store.append({
                "id": event_id,
                "type": "chat_turn",
                "timestamp": timestamp,
            })

        assert [event["id"] for event in store.load_recent(2)] == ["event-2", "event-3"]
    finally:
        db.close()


def test_sqlite_event_log_loads_bounded_recent_window_and_updates(tmp_path):
    legacy_path = tmp_path / "events.json"
    legacy_path.write_text(json.dumps([
        {
            "id": f"event-{i}",
            "type": "chat_turn",
            "timestamp": f"2026-05-22T09:0{i}:00+00:00",
            "text": f"message {i}",
        }
        for i in range(4)
    ]))

    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteEventStore(db, legacy_path=legacy_path)
        log = EventLog(legacy_path, max_events=2, store=store)
        assert [event["id"] for event in log.get_events()] == ["event-2", "event-3"]
        assert log.update_event("event-3", {"status": "done"}) is True

        reloaded = EventLog(legacy_path, max_events=4, store=store)
        assert reloaded.get_events()[-1]["status"] == "done"
    finally:
        db.close()


@pytest.mark.asyncio
async def test_create_test_app_uses_sqlite_event_log_without_json_write(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        app.state.event_log.add_event("shutdown", {"text": "persist me"})
        assert app.state.event_log.dirty is False

    assert not (tmp_path / "events.json").exists()
    db = StateDatabase(tmp_path / "state.db")
    try:
        row = db.conn.execute("SELECT payload_json FROM events").fetchone()
        assert row is not None
        assert json.loads(row["payload_json"])["text"] == "persist me"
    finally:
        db.close()


@pytest.mark.asyncio
async def test_sqlite_event_log_seeds_memory_after_restart(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with app.router.lifespan_context(app):
        event_id = app.state.event_log.add_event("restart", {"text": "survive"})

    restarted = create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with restarted.router.lifespan_context(restarted):
        events = restarted.state.event_log.get_events()
        assert len(events) == 1
        assert events[0]["id"] == event_id
        assert events[0]["text"] == "survive"
