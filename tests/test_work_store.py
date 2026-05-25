from __future__ import annotations

from pathlib import Path

from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.work import SQLiteWorkStore


def test_sqlite_work_store_create_list_and_reload(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create(
            title="Run checks",
            kind="verification",
            created_by_peer_id="repow-default-creator",
            owner_peer_id="repow-default-owner",
            assigned_peer_id="repow-default-worker",
            repowire_session_id="rw-session-1",
            correlation_id="ask-1",
            circle="default",
            source_kind="mcp",
            source_id="tool-call-1",
            scope="repo",
            request={"task": "run checks"},
            provenance={"links": {"ask_id": "ask-1"}},
        )

        assert work.work_id.startswith("work-")
        assert work.status()["job_id"] == work.work_id
        assert work.title == "Run checks"
        assert work.kind == "verification"
        assert work.state == "queued"
        assert work.status()["links"] == {"ask_id": "ask-1"}
        assert [item.work_id for item in store.list_all(owner_peer_id="repow-default-owner")] == [
            work.work_id,
        ]
    finally:
        db.close()

    reopened = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(reopened)
        loaded = store.list_all(repowire_session_id="rw-session-1")[0]
        assert loaded.work_id == work.work_id
        assert loaded.request == {"task": "run checks"}
        assert loaded.source_kind == "mcp"
        assert loaded.assigned_peer_id == "repow-default-worker"
        assert loaded.correlation_id == "ask-1"
    finally:
        reopened.close()


def test_sqlite_work_store_result_not_ready_until_terminal(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create(created_by_peer_id="repow-default-creator")

        assert store.get(work.work_id).result()["result_state"] == "not_ready"  # type: ignore[union-attr]

        completed = store.update_state(
            work.work_id,
            state="completed",
            progress_note="checks finished",
            result_summary="checks passed",
            result_data={"passed": 12},
            artifacts=[{"path": "logs/checks.txt"}],
        )

        assert completed is not None
        result = completed.result()
        assert result["state"] == "completed"
        assert result["job_id"] == work.work_id
        assert result["summary"] == "checks passed"
        assert result["data"] == {"passed": 12}
        assert result["completed_at"] is not None
        assert completed.progress_events[0]["note"] == "checks finished"
    finally:
        db.close()


def test_sqlite_work_store_cancel_queued_work_is_terminal(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create(owner_peer_id="repow-default-owner")

        cancelled = store.cancel(
            work.work_id,
            requested_by_peer_id="repow-default-creator",
        )

        assert cancelled is not None
        assert cancelled.state == "cancelled"
        assert cancelled.state_reason == "cancel_requested"
        assert cancelled.cancel_requested is True
        assert cancelled.cancel_requested_by_peer_id == "repow-default-creator"
        assert cancelled.cancellation_reason == "cancel_requested"
        assert cancelled.completed_at is not None
    finally:
        db.close()


def test_sqlite_work_store_cancel_running_work_records_pending_cancel(tmp_path: Path) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create(owner_peer_id="repow-default-owner")
        running = store.update_state(work.work_id, state="running", phase="tests")
        assert running is not None

        cancelled = store.cancel(work.work_id, reason="user_requested")

        assert cancelled is not None
        assert cancelled.state == "running"
        assert cancelled.state_reason == "user_requested"
        assert cancelled.cancel_requested is True
        assert cancelled.completed_at is None
    finally:
        db.close()


def test_sqlite_work_store_terminal_state_cannot_move_back_to_non_terminal(
    tmp_path: Path,
) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create()
        completed = store.update_state(
            work.work_id,
            state="completed",
            result_summary="done",
            result_data={"ok": True},
            error={"code": "none"},
            artifacts=[{"path": "artifact.txt"}],
        )
        assert completed is not None

        try:
            store.update_state(work.work_id, state="running")
        except ValueError as e:
            assert "terminal state cannot be changed" in str(e)
        else:
            raise AssertionError("expected terminal state guard")

        still_completed = store.get(work.work_id)
        assert still_completed is not None
        assert still_completed.state == "completed"
    finally:
        db.close()


def test_sqlite_work_store_terminal_metadata_update_preserves_omitted_result_fields(
    tmp_path: Path,
) -> None:
    db = StateDatabase(tmp_path / "state.db")
    try:
        store = SQLiteWorkStore(db)
        work = store.create()
        completed = store.update_state(
            work.work_id,
            state="failed",
            result_summary="failed in setup",
            result_data={"step": "setup"},
            error={"message": "missing config"},
            artifacts=[{"path": "logs/setup.txt"}],
        )
        assert completed is not None

        enriched = store.update_state(
            work.work_id,
            state="failed",
            progress_note="triaged",
        )

        assert enriched is not None
        assert enriched.state == "failed"
        assert enriched.result_summary == "failed in setup"
        assert enriched.result_data == {"step": "setup"}
        assert enriched.error == {"message": "missing config"}
        assert enriched.artifacts == [{"path": "logs/setup.txt"}]
        assert enriched.progress_events[-1]["note"] == "triaged"
    finally:
        db.close()
