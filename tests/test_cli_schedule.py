from __future__ import annotations

from unittest.mock import MagicMock

from click.testing import CliRunner

from repowire.cli import main


def _mock_client(monkeypatch, response_json: dict | None = None) -> MagicMock:
    response = MagicMock()
    response.status_code = 200
    response.json.return_value = response_json or {
        "schedule_id": "sched-abc123",
        "from_peer": "me",
        "to_peer": "me",
        "text": "wake",
        "fire_at": "2026-05-19T09:00:00+00:00",
        "kind": "notify",
        "circle": None,
        "cron": None,
        "created_at": "2026-05-19T08:00:00+00:00",
    }
    response.raise_for_status.return_value = None
    client = MagicMock()
    client.post.return_value = response
    client.get.return_value = response
    client.delete.return_value = response
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")
    return client


def test_schedule_self_posts_to_self_with_cron(monkeypatch) -> None:
    monkeypatch.setenv("REPOWIRE_DISPLAY_NAME", "me")
    client = _mock_client(monkeypatch, response_json={
        "schedule_id": "sched-abc123",
        "from_peer": "me",
        "to_peer": "me",
        "text": "stand up",
        "fire_at": "2026-05-19T09:00:00+00:00",
        "kind": "notify",
        "circle": None,
        "cron": "*/15 * * * *",
        "created_at": "2026-05-19T08:00:00+00:00",
    })

    result = CliRunner().invoke(
        main,
        ["schedule", "self", "--cron", "*/15 * * * *", "stand up"],
    )

    assert result.exit_code == 0, result.output
    client.post.assert_called_once()
    assert client.post.call_args.kwargs["json"] == {
        "from_peer": "me",
        "to_peer": "me",
        "text": "stand up",
        "kind": "notify",
        "cron": "*/15 * * * *",
    }
    assert "sched-abc123" in result.output


def test_schedule_create_posts_one_shot_with_from_peer(monkeypatch) -> None:
    client = _mock_client(monkeypatch)

    result = CliRunner().invoke(
        main,
        ["schedule", "create", "bob", "10m", "ping", "--from-peer", "alice"],
    )

    assert result.exit_code == 0, result.output
    body = client.post.call_args.kwargs["json"]
    assert body["from_peer"] == "alice"
    assert body["to_peer"] == "bob"
    assert body["text"] == "ping"
    assert body["kind"] == "notify"
    assert "fire_at" in body
    assert "cron" not in body


def test_schedule_delete_calls_route(monkeypatch) -> None:
    client = _mock_client(monkeypatch)

    result = CliRunner().invoke(main, ["schedule", "delete", "sched-abc123"])

    assert result.exit_code == 0, result.output
    client.delete.assert_called_once_with(
        "http://127.0.0.1:8377/schedules/sched-abc123",
    )
