from __future__ import annotations

from unittest.mock import MagicMock

import httpx
from click.testing import CliRunner

from repowire.cli import main


def _status(job_id: str = "work-abc123") -> dict:
    return {
        "job_id": job_id,
        "work_id": job_id,
        "title": "Run checks",
        "kind": "verification",
        "state": "queued",
        "owner_peer_id": "owner",
        "updated_at": "2026-05-25T10:00:00+00:00",
    }


def _mock_client(monkeypatch, response_json: dict | None = None) -> MagicMock:
    response = MagicMock()
    response.status_code = 200
    response.json.return_value = response_json or {
        "job_id": "work-abc123",
        "work_id": "work-abc123",
        "status": _status(),
    }
    response.raise_for_status.return_value = None
    client = MagicMock()
    client.post.return_value = response
    client.get.return_value = response
    client.patch.return_value = response
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")
    return client


def test_jobs_create_posts_job_metadata(monkeypatch) -> None:
    client = _mock_client(monkeypatch)

    result = CliRunner().invoke(
        main,
        [
            "jobs",
            "create",
            "Run checks",
            "--kind",
            "verification",
            "--owner",
            "owner",
            "--circle",
            "default",
        ],
    )

    assert result.exit_code == 0, result.output
    client.post.assert_called_once_with(
        "http://127.0.0.1:8377/jobs",
        json={
            "title": "Run checks",
            "kind": "verification",
            "source_kind": "cli",
            "owner_peer_id": "owner",
            "circle": "default",
        },
    )
    assert "work-abc123" in result.output


def test_jobs_create_json_emits_status_json(monkeypatch) -> None:
    _mock_client(monkeypatch)

    result = CliRunner().invoke(main, ["jobs", "create", "Run checks", "--json"])

    assert result.exit_code == 0, result.output
    assert '"job_id": "work-abc123"' in result.output
    assert '"title": "Run checks"' in result.output


def test_jobs_list_filters_and_renders_table(monkeypatch) -> None:
    client = _mock_client(monkeypatch, response_json={"work": [_status()]})

    result = CliRunner().invoke(main, ["jobs", "list", "--state", "queued"])

    assert result.exit_code == 0, result.output
    client.get.assert_called_once_with(
        "http://127.0.0.1:8377/jobs",
        params={"state": "queued"},
    )
    assert "Run checks" in result.output


def test_jobs_list_json_emits_work_array(monkeypatch) -> None:
    client = _mock_client(monkeypatch, response_json={"work": [_status()]})

    result = CliRunner().invoke(main, ["jobs", "list", "--json"])

    assert result.exit_code == 0, result.output
    client.get.assert_called_once_with("http://127.0.0.1:8377/jobs", params=None)
    assert '"work"' in result.output
    assert '"job_id": "work-abc123"' in result.output


def test_jobs_show_404_exits_nonzero(monkeypatch) -> None:
    response = MagicMock()
    response.status_code = 404
    client = MagicMock()
    client.get.return_value = response
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")

    result = CliRunner().invoke(main, ["jobs", "show", "work-missing"])

    assert result.exit_code != 0
    assert "No job: work-missing" in result.output


def test_jobs_list_connection_error_exits_nonzero(monkeypatch) -> None:
    client = MagicMock()
    client.get.side_effect = httpx.ConnectError("no daemon")
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")

    result = CliRunner().invoke(main, ["jobs", "list"])

    assert result.exit_code != 0
    assert "Cannot connect to daemon" in result.output


def test_jobs_update_patches_status(monkeypatch) -> None:
    client = _mock_client(monkeypatch)

    result = CliRunner().invoke(
        main,
        [
            "jobs",
            "update",
            "work-abc123",
            "--state",
            "completed",
            "--note",
            "started",
            "--attempt-id",
            "attempt-1",
        ],
    )

    assert result.exit_code == 0, result.output
    client.patch.assert_called_once_with(
        "http://127.0.0.1:8377/jobs/work-abc123",
        json={"state": "completed", "progress_note": "started", "attempt_id": "attempt-1"},
    )


def test_jobs_update_json_emits_status(monkeypatch) -> None:
    _mock_client(monkeypatch)

    result = CliRunner().invoke(
        main,
        ["jobs", "update", "work-abc123", "--state", "running", "--json"],
    )

    assert result.exit_code == 0, result.output
    assert '"job_id": "work-abc123"' in result.output


def test_jobs_cancel_posts_cancel_request(monkeypatch) -> None:
    client = _mock_client(monkeypatch)

    result = CliRunner().invoke(
        main,
        ["jobs", "cancel", "work-abc123", "--reason", "stale"],
    )

    assert result.exit_code == 0, result.output
    client.post.assert_called_once_with(
        "http://127.0.0.1:8377/jobs/work-abc123/cancel",
        json={"reason": "stale"},
    )


def test_jobs_cancel_http_error_exits_nonzero(monkeypatch) -> None:
    request = httpx.Request("POST", "http://127.0.0.1:8377/jobs/work-abc123/cancel")
    response = httpx.Response(500, json={"detail": "boom"}, request=request)
    client = MagicMock()
    client.post.return_value = response
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")

    result = CliRunner().invoke(main, ["jobs", "cancel", "work-abc123"])

    assert result.exit_code != 0
    assert "Failed to cancel job: boom" in result.output


def test_jobs_result_prints_json(monkeypatch) -> None:
    client = _mock_client(
        monkeypatch,
        response_json={"result": {"job_id": "work-abc123", "result_state": "not_ready"}},
    )

    result = CliRunner().invoke(main, ["jobs", "result", "work-abc123", "--json"])

    assert result.exit_code == 0, result.output
    client.get.assert_called_once_with("http://127.0.0.1:8377/jobs/work-abc123/result")
    assert "not_ready" in result.output
