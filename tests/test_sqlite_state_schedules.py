"""SQLite state DB and schedule adapter migration tests."""

from __future__ import annotations

import json
from dataclasses import asdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

from repowire.config.models import Config
from repowire.daemon.schedule_store import Schedule, ScheduleStore
from repowire.daemon.state.database import SCHEMA_VERSION, StateDatabase
from repowire.daemon.state.schedules import SQLiteScheduleStore


def _ts(seconds_from_now: float = 60.0) -> datetime:
    return datetime.now(timezone.utc) + timedelta(seconds=seconds_from_now)


def test_state_database_migration_idempotent_and_pragmas(tmp_path: Path) -> None:
    db_path = tmp_path / "state.db"
    db = StateDatabase(db_path)
    try:
        assert db.integrity_check() == "ok"
        assert db.conn.execute("PRAGMA user_version").fetchone()[0] == SCHEMA_VERSION
        assert db.conn.execute("PRAGMA foreign_keys").fetchone()[0] == 1
        assert db.conn.execute("PRAGMA busy_timeout").fetchone()[0] == 5000
        assert db.conn.execute("SELECT COUNT(*) FROM schema_migrations").fetchone()[0] == 1
    finally:
        db.close()

    db2 = StateDatabase(db_path)
    try:
        assert db2.conn.execute("PRAGMA user_version").fetchone()[0] == SCHEMA_VERSION
        assert db2.conn.execute("SELECT COUNT(*) FROM schema_migrations").fetchone()[0] == 1
    finally:
        db2.close()


def test_sqlite_schedule_store_create_list_delete_parity(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteScheduleStore(db)
        later = store.create("alice", "bob", "later", _ts(120), kind="ask", circle="dev")
        sooner = store.create("alice", "bob", "sooner", _ts(60))
        store.create("eve", "bob", "third-party", _ts(30))

        assert store.get(later.schedule_id) is not None
        assert [s.schedule_id for s in store.list_all("alice")] == [
            sooner.schedule_id,
            later.schedule_id,
        ]
        assert store.next_due() is not None
        removed = store.delete(later.schedule_id)
        assert removed is not None
        assert removed.kind == "ask"
        assert removed.circle == "dev"
        assert store.get(later.schedule_id) is None
    finally:
        db.close()


def test_sqlite_schedule_store_reschedule_cron_persists(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteScheduleStore(db)
        now = datetime(2026, 5, 19, 8, 10, tzinfo=timezone.utc)
        sched = store.create_cron("alice", "alice", "stand up", "*/15 * * * *", now=now)
        assert sched.fire_at == datetime(2026, 5, 19, 8, 15, tzinfo=timezone.utc).isoformat()
        assert store.reschedule_next(
            sched.schedule_id,
            after=datetime(2026, 5, 19, 8, 15, tzinfo=timezone.utc),
        )
        reloaded = store.get(sched.schedule_id)
        assert reloaded is not None
        assert reloaded.fire_at == datetime(2026, 5, 19, 8, 30, tzinfo=timezone.utc).isoformat()
    finally:
        db.close()


def test_sqlite_schedule_store_imports_legacy_schedules_json_once(tmp_path: Path) -> None:
    legacy_path = tmp_path / "schedules.json"
    json_store = ScheduleStore(legacy_path)
    legacy = json_store.create("alice", "bob", "ping", _ts(), kind="notify", circle="default")
    json_store.persist()

    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteScheduleStore(db, legacy_path=legacy_path)
        imported = store.get(legacy.schedule_id)
        assert imported is not None
        assert imported.from_peer == "alice"
        assert imported.circle == "default"
        assert legacy_path.exists(), "legacy JSON is left untouched for downgrade/backcompat"
        assert db.conn.execute("SELECT row_count FROM legacy_imports").fetchone()[0] == 1

        store.create("new", "peer", "new state", _ts())
        store2 = SQLiteScheduleStore(db, legacy_path=legacy_path)
        assert len(store2.list_all()) == 2
        assert db.conn.execute("SELECT COUNT(*) FROM legacy_imports").fetchone()[0] == 1
    finally:
        db.close()


def test_sqlite_schedule_store_corrupt_legacy_json_records_error_and_starts_empty(
    tmp_path: Path,
) -> None:
    legacy_path = tmp_path / "schedules.json"
    legacy_path.write_text("{not json")
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteScheduleStore(db, legacy_path=legacy_path)
        assert store.list_all() == []
        row = db.conn.execute("SELECT status, row_count, error FROM legacy_imports").fetchone()
        assert row[0] == "error"
        assert row[1] == 0
        assert row[2] == "JSONDecodeError"
    finally:
        db.close()

    reopened = StateDatabase(tmp_path / "state.db")
    try:
        row = reopened.conn.execute(
            "SELECT status, row_count, error FROM legacy_imports",
        ).fetchone()
        assert row[0] == "error"
        assert row[1] == 0
        assert row[2] == "JSONDecodeError"
    finally:
        reopened.close()


def test_sqlite_schedule_store_imports_existing_legacy_shape(tmp_path: Path) -> None:
    legacy_path = tmp_path / "schedules.json"
    sched = Schedule(
        schedule_id="sched-legacy",
        from_peer="alice",
        to_peer="bob",
        text="old shape",
        fire_at=_ts().isoformat(),
    )
    legacy_path.write_text(json.dumps({sched.schedule_id: asdict(sched)}))

    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteScheduleStore(db, legacy_path=legacy_path)
        assert store.export_json_compatible()[sched.schedule_id]["text"] == "old shape"
    finally:
        db.close()


async def test_create_app_uses_sqlite_schedule_store_and_imports_legacy_json(
    monkeypatch,
    tmp_path: Path,
) -> None:
    from repowire.daemon import app as app_mod

    legacy_dir = tmp_path / ".repowire"
    legacy_dir.mkdir()
    legacy_path = legacy_dir / "schedules.json"
    sched = Schedule(
        schedule_id="sched-app-legacy",
        from_peer="alice",
        to_peer="bob",
        text="from legacy",
        fire_at=_ts().isoformat(),
    )
    legacy_path.write_text(json.dumps({sched.schedule_id: asdict(sched)}))
    monkeypatch.setattr(Path, "home", lambda: tmp_path)

    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_app(config=cfg, install_tmux_hooks=False)
    async with app.router.lifespan_context(app):
        assert isinstance(app.state.schedule_store, SQLiteScheduleStore)
        imported = app.state.schedule_store.get(sched.schedule_id)
        assert imported is not None
        assert imported.text == "from legacy"
        assert (legacy_dir / "state.db").exists()

    reopened = StateDatabase(legacy_dir / "state.db")
    try:
        assert reopened.conn.execute("SELECT row_count FROM legacy_imports").fetchone()[0] == 1
    finally:
        reopened.close()
