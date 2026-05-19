"""Tests for ScheduleStore: in-memory + persistence behavior."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from repowire.daemon.schedule_store import ScheduleStore


def _ts(seconds_from_now: float = 60.0) -> datetime:
    return datetime.now(timezone.utc) + timedelta(seconds=seconds_from_now)


def test_create_assigns_id_and_marks_dirty(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    sched = store.create(
        from_peer="alice", to_peer="bob", text="ping", fire_at=_ts(),
    )
    assert sched.schedule_id.startswith("sched-")
    assert store.get(sched.schedule_id) is sched


def test_create_rejects_naive_datetime(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    with pytest.raises(ValueError):
        store.create(
            from_peer="alice", to_peer="bob", text="x",
            fire_at=datetime(2030, 1, 1),
        )


def test_create_rejects_unknown_kind(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    with pytest.raises(ValueError):
        store.create(
            from_peer="alice", to_peer="bob", text="x",
            fire_at=_ts(), kind="recurring",
        )


def test_delete_returns_schedule_and_removes(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    sched = store.create("a", "b", "t", _ts())
    removed = store.delete(sched.schedule_id)
    assert removed is sched
    assert store.get(sched.schedule_id) is None
    assert store.delete(sched.schedule_id) is None


def test_list_all_filters_by_from_peer_and_sorts(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    later = store.create("alice", "bob", "later", _ts(120))
    sooner = store.create("alice", "bob", "sooner", _ts(60))
    store.create("eve", "bob", "third-party", _ts(30))

    mine = store.list_all(from_peer="alice")
    assert [s.schedule_id for s in mine] == [sooner.schedule_id, later.schedule_id]
    all_ids = {s.schedule_id for s in store.list_all()}
    assert len(all_ids) == 3


def test_next_due_returns_earliest(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    assert store.next_due() is None
    store.create("a", "b", "late", _ts(300))
    early = store.create("a", "b", "early", _ts(10))
    nxt = store.next_due()
    assert nxt is not None and nxt.schedule_id == early.schedule_id


def test_persist_roundtrip(tmp_path: Path) -> None:
    path = tmp_path / "schedules.json"
    s1 = ScheduleStore(path)
    sched = s1.create("alice", "bob", "ping", _ts(60), kind="ask", circle="default")
    s1.persist()
    assert path.exists()

    s2 = ScheduleStore(path)
    loaded = s2.get(sched.schedule_id)
    assert loaded is not None
    assert loaded.from_peer == "alice"
    assert loaded.to_peer == "bob"
    assert loaded.kind == "ask"
    assert loaded.circle == "default"


def test_create_cron_sets_next_fire_and_persists_expression(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    now = datetime(2026, 5, 19, 8, 10, tzinfo=timezone.utc)
    sched = store.create_cron(
        "alice", "alice", "stand up", "*/15 * * * *", now=now,
    )
    assert sched.cron == "*/15 * * * *"
    assert sched.fire_at == datetime(2026, 5, 19, 8, 15, tzinfo=timezone.utc).isoformat()


def test_reschedule_next_advances_recurring_schedule(tmp_path: Path) -> None:
    store = ScheduleStore(tmp_path / "schedules.json")
    now = datetime(2026, 5, 19, 8, 10, tzinfo=timezone.utc)
    sched = store.create_cron("alice", "alice", "ping", "@hourly", now=now)
    ok = store.reschedule_next(
        sched.schedule_id,
        after=datetime(2026, 5, 19, 9, 0, tzinfo=timezone.utc),
    )
    assert ok is True
    assert sched.fire_at == datetime(2026, 5, 19, 10, 0, tzinfo=timezone.utc).isoformat()


def test_persist_noop_when_clean(tmp_path: Path) -> None:
    path = tmp_path / "schedules.json"
    store = ScheduleStore(path)
    store.persist()  # nothing to write
    assert not path.exists()


def test_corrupt_file_recovers_empty(tmp_path: Path) -> None:
    path = tmp_path / "schedules.json"
    path.write_text("{not json")
    store = ScheduleStore(path)
    assert store.list_all() == []
