"""SQLite state DB and schedule adapter migration tests."""

from __future__ import annotations

import json
from dataclasses import asdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.schedule_store import Schedule, ScheduleStore
from repowire.daemon.state.database import SCHEMA_VERSION, StateDatabase
from repowire.daemon.state.schedules import SQLiteScheduleStore
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore


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
        versions = {
            row[0]
            for row in db.conn.execute(
                "SELECT version FROM schema_migrations",
            ).fetchall()
        }
        assert versions == {1, 2, 3, 4, 5}
        tables = {
            row[0]
            for row in db.conn.execute(
                "SELECT name FROM sqlite_master WHERE type = 'table'",
            ).fetchall()
        }
        assert "session_bindings" in tables
        assert "events" in tables
        assert "peer_session_mappings" in tables
        assert "runtime_identity_certificates" in tables
    finally:
        db.close()

    db2 = StateDatabase(db_path)
    try:
        assert db2.conn.execute("PRAGMA user_version").fetchone()[0] == SCHEMA_VERSION
        versions = {
            row[0]
            for row in db2.conn.execute(
                "SELECT version FROM schema_migrations",
            ).fetchall()
        }
        assert versions == {1, 2, 3, 4, 5}
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


def test_sqlite_session_binding_store_upsert_and_lookup_fields(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteSessionBindingStore(db)
        binding = store.upsert_observation(
            peer_id="repow-default-a1",
            backend="claude-code",
            project_path="/repo",
            runtime_session_id="hook-session-1",
            runtime_source_uri="claude-jsonl:repo/hook-session-1.jsonl",
            source_cursor={"line_offset": 7},
            provenance={"source_kind": "runtime_transcript"},
            metadata={"hook_session_id": "hook-session-1"},
        )

        updated = store.upsert_observation(
            peer_id="repow-default-a1",
            backend="claude-code",
            project_path="/repo",
            runtime_session_id="hook-session-1",
            metadata={"last_turn_id": "turn-9"},
        )

        assert updated.repowire_session_id == binding.repowire_session_id
        assert updated.current_executor_peer_id == "repow-default-a1"
        assert updated.runtime_source_uri == "claude-jsonl:repo/hook-session-1.jsonl"
        assert updated.source_cursor == {"line_offset": 7}
        assert updated.metadata["hook_session_id"] == "hook-session-1"
        assert updated.metadata["last_turn_id"] == "turn-9"
        assert store.get(binding.repowire_session_id) is not None
        assert store.list_by_peer("repow-default-a1")[0].runtime_session_id == "hook-session-1"
        assert (
            store.get_by_runtime_session(
                "hook-session-1",
                backend="claude-code",
                project_path="/repo",
            )
            is not None
        )
        assert (
            store.list_by_backend_project(backend="claude-code", project_path="/repo")[0]
            .repowire_session_id
            == binding.repowire_session_id
        )
        by_source = store.get_by_source_uri("claude-jsonl:repo/hook-session-1.jsonl")
        assert by_source is not None
        assert by_source.repowire_session_id == binding.repowire_session_id
    finally:
        db.close()


def test_sqlite_session_binding_store_mints_and_validates_birth_certificate(
    tmp_path: Path,
) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteSessionBindingStore(db)
        cert = store.mint_birth_certificate(
            peer_id="repow-default-a1",
            display_name="repo-claude-code",
            backend="claude-code",
            project_path="/repo",
            runtime_session_id="runtime-1",
            pane_id="%77",
            agent_pid=12345,
            parent_pid=1,
            metadata={"circle": "default"},
            issued_at=datetime.now(timezone.utc).isoformat(),
        )

        validated = store.validate_birth_certificate(
            cert.as_envelope(),
            backend="claude-code",
            project_path="/repo",
            pane_id="%77",
            agent_pid=12345,
        )
        assert validated is not None
        assert validated.peer_id == "repow-default-a1"

        assert store.validate_birth_certificate(
            {**cert.as_envelope(), "backend": "codex"},
            backend="codex",
            project_path="/repo",
            pane_id="%77",
            agent_pid=12345,
        ) is None
        assert store.validate_birth_certificate(
            cert.as_envelope(),
            backend="claude-code",
            project_path="/repo",
            pane_id="%99",
            agent_pid=12345,
        ) is None
        assert store.validate_birth_certificate(
            cert.as_envelope(),
            backend="claude-code",
            project_path="/repo",
            pane_id="%77",
            agent_pid=99999,
        ) is None
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


async def test_test_app_wires_session_binding_store_and_observes_hooks(tmp_path: Path) -> None:
    from repowire.daemon import app as app_mod

    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")
    async with app.router.lifespan_context(app):
        assert isinstance(app.state.session_binding_store, SQLiteSessionBindingStore)
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            reg = await client.post(
                "/peers",
                json={
                    "name": "worker",
                    "path": "/repo",
                    "circle": "default",
                    "backend": "claude-code",
                    "pane_id": "%77",
                    "metadata": {
                        "hook_session_id": "hook-session-1",
                        "runtime_source_uri": "claude-jsonl:repo/hook-session-1.jsonl",
                    },
                },
            )
            assert reg.status_code == 200
            reg_body = reg.json()
            peer_id = reg_body["peer_id"]
            birth_certificate = reg_body["birth_certificate"]
            assert birth_certificate["peer_id"] == peer_id
            assert birth_certificate["runtime_session_id"] == "hook-session-1"
            assert birth_certificate["pane_id"] == "%77"

            binding = app.state.session_binding_store.get_by_runtime_session(
                "hook-session-1",
                backend="claude-code",
                project_path="/repo",
            )
            assert binding is not None
            assert binding.peer_id == peer_id
            assert binding.metadata["hook_session_id"] == "hook-session-1"

            validated = await client.post(
                "/peers/identity/validate",
                json={
                    "birth_certificate": birth_certificate,
                    "backend": "claude-code",
                    "path": "/repo",
                    "pane_id": "%77",
                },
            )
            assert validated.status_code == 200
            assert validated.json()["peer_id"] == peer_id

            app.state.peer_registry._peers.clear()
            rehydrated = await client.post(
                "/peers/identity/validate",
                json={
                    "birth_certificate": birth_certificate,
                    "backend": "claude-code",
                    "path": "/repo",
                    "pane_id": "%77",
                },
            )
            assert rehydrated.status_code == 200
            assert rehydrated.json()["peer_id"] == peer_id
            assert rehydrated.json()["peer"]["display_name"] == "repo-claude-code"

            chat = await client.post(
                "/events/chat",
                json={
                    "peer": "worker-claude-code",
                    "role": "assistant",
                    "text": "done",
                    "session_id": "hook-session-1",
                    "turn_id": "turn-1",
                    "pane_id": "%77",
                },
            )
            assert chat.status_code == 200

            updated = app.state.session_binding_store.get_by_runtime_session(
                "hook-session-1",
                backend="claude-code",
                project_path="/repo",
            )
            assert updated is not None
            assert updated.repowire_session_id == binding.repowire_session_id
            assert updated.metadata["last_turn_id"] == "turn-1"
            assert updated.provenance["source_event_id"] == "turn-1"
