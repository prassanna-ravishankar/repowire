"""CLI tests for agy-mesh fallback commands: peer whoami / asks / ack."""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import MagicMock

from click.testing import CliRunner

from repowire.cli import main


def _make_client(monkeypatch) -> MagicMock:
    client = MagicMock()
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")
    return client


def _set_auth_token(monkeypatch, token: str = "secret-token") -> None:
    monkeypatch.setattr(
        "repowire.config.models.load_config",
        lambda: SimpleNamespace(daemon=SimpleNamespace(auth_token=token)),
    )


def _response(status: int = 200, json_body: dict | list | None = None, text: str = "") -> MagicMock:
    resp = MagicMock()
    resp.status_code = status
    resp.json.return_value = json_body if json_body is not None else {}
    resp.text = text
    if status >= 400:
        resp.raise_for_status.side_effect = Exception(f"http {status}")
    else:
        resp.raise_for_status.return_value = None
    return resp


def _http_status_error(response: MagicMock):
    import httpx

    request = httpx.Request("POST", "http://127.0.0.1:8377/test")
    raw = httpx.Response(response.status_code, request=request, text=response.text)
    raw._json = response.json.return_value  # type: ignore[attr-defined]
    raw.json = response.json
    return httpx.HTTPStatusError("error", request=request, response=raw)


# --- peer whoami ---------------------------------------------------------------


def test_whoami_reads_pane(monkeypatch) -> None:
    monkeypatch.setenv("TMUX_PANE", "%42")
    client = _make_client(monkeypatch)
    client.get.return_value = _response(
        200,
        {
            "peer_id": "p-1",
            "display_name": "agy-demo",
            "backend": "antigravity",
            "circle": "default",
            "status": "online",
        },
    )

    result = CliRunner().invoke(main, ["peer", "whoami"])

    assert result.exit_code == 0, result.output
    assert "p-1" in result.output
    assert "agy-demo" in result.output
    assert "/peers/by-pane/" in client.get.call_args.args[0]


def test_whoami_register_posts_with_backend_and_pane(monkeypatch) -> None:
    monkeypatch.setenv("TMUX_PANE", "%9")
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200, {"peer_id": "p-99", "display_name": "agy-demo", "ok": True},
    )

    result = CliRunner().invoke(
        main,
        ["peer", "whoami", "--register", "--backend", "antigravity",
         "--name", "agy-demo", "--circle", "default", "--path", "/tmp/x"],
    )

    assert result.exit_code == 0, result.output
    body = client.post.call_args.kwargs["json"]
    assert body == {
        "name": "agy-demo",
        "path": "/tmp/x",
        "backend": "antigravity",
        "metadata": {"repowire_cli_fallback": True},
        "circle": "default",
        "pane_id": "%9",
    }
    assert "p-99" in result.output


def test_whoami_register_uses_config_auth_token(monkeypatch) -> None:
    _set_auth_token(monkeypatch)
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200, {"peer_id": "p-99", "display_name": "agy-demo"},
    )

    result = CliRunner().invoke(
        main,
        ["peer", "whoami", "--register", "--backend", "antigravity", "--name", "agy-demo"],
    )

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["headers"] == {"Authorization": "Bearer secret-token"}


def test_whoami_register_requires_backend(monkeypatch) -> None:
    _make_client(monkeypatch)

    result = CliRunner().invoke(main, ["peer", "whoami", "--register"])

    assert result.exit_code == 2
    assert "--backend" in result.output


def test_whoami_no_pane_no_name_exits_nonzero(monkeypatch) -> None:
    monkeypatch.delenv("TMUX_PANE", raising=False)
    _make_client(monkeypatch)

    result = CliRunner().invoke(main, ["peer", "whoami"])

    assert result.exit_code == 1
    assert "No registered peer" in result.output


# --- peer asks -----------------------------------------------------------------


def test_peer_ask_uses_client_query_and_prints_reply(monkeypatch) -> None:
    calls: list[dict] = []

    class FakeAsyncRepowireClient:
        def __init__(self, *, base_url, auth_token, timeout):
            calls.append({
                "base_url": base_url,
                "auth_token": auth_token,
                "timeout": timeout,
            })

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc_info):
            return None

        async def query(self, to_peer, text, *, timeout=None, circle=None):
            calls.append({
                "to_peer": to_peer,
                "text": text,
                "query_timeout": timeout,
                "circle": circle,
            })
            return SimpleNamespace(text="pong", error=None)

    _set_auth_token(monkeypatch)
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://daemon")
    monkeypatch.setattr("repowire.client.AsyncRepowireClient", FakeAsyncRepowireClient)

    result = CliRunner().invoke(
        main,
        ["peer", "ask", "worker", "ping", "--timeout", "7", "--circle", "team-a"],
    )

    assert result.exit_code == 0, result.output
    assert calls == [
        {"base_url": "http://daemon", "auth_token": "secret-token", "timeout": 12.0},
        {
            "to_peer": "worker",
            "text": "ping",
            "query_timeout": 7,
            "circle": "team-a",
        },
    ]
    assert "worker:" in result.output
    assert "pong" in result.output


def test_peer_ask_prints_query_error(monkeypatch) -> None:
    class FakeAsyncRepowireClient:
        def __init__(self, **_):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc_info):
            return None

        async def query(self, *_args, **_kwargs):
            return SimpleNamespace(text=None, error="Peer 'worker' is busy")

    monkeypatch.setattr("repowire.client.AsyncRepowireClient", FakeAsyncRepowireClient)

    result = CliRunner().invoke(main, ["peer", "ask", "worker", "ping"])

    assert result.exit_code == 0, result.output
    assert "Error: Peer 'worker' is busy" in result.output


def test_peer_restart_posts_request_and_prints_response(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200,
        {
            "ok": True,
            "status": "restarted",
            "restarted": True,
            "peer_id": "repow-default-abc12345",
            "display_name": "proj-claude-code",
            "backend": "claude-code",
            "path": "/tmp/proj",
            "circle": "default",
            "tmux_session": "default:proj",
            "resume_mode": "resumed",
            "command": "claude",
        },
    )

    result = CliRunner().invoke(
        main,
        [
            "peer",
            "restart",
            "proj-claude-code",
            "--circle",
            "default",
            "--from-peer",
            "dashboard",
            "--message",
            "reload context",
        ],
    )

    assert result.exit_code == 0, result.output
    assert client.post.call_args.args[0].endswith("/peers/proj-claude-code/restart")
    assert client.post.call_args.kwargs["json"] == {
        "dry_run": False,
        "circle": "default",
        "from_peer": "dashboard",
        "message": "reload context",
    }
    assert "Restarted" in result.output
    assert "resumed" in result.output
    assert "default:proj" in result.output


def test_peer_restart_dry_run_output(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200,
        {
            "ok": True,
            "status": "restart_available",
            "restarted": False,
            "peer_id": "repow-default-abc12345",
            "display_name": "proj-claude-code",
            "backend": "claude-code",
            "path": "/tmp/proj",
            "circle": "default",
            "tmux_session": "default:proj",
            "resume_mode": "resumed",
        },
    )

    result = CliRunner().invoke(main, ["peer", "restart", "proj-claude-code", "--dry-run"])

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["json"] == {"dry_run": True}
    assert "Restart available" in result.output


def test_peer_restart_prints_http_error_detail(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    response = _response(
        409,
        {
            "detail": {
                "error": "unsupported_pane_ownership",
                "hint": "Restart only supports daemon-spawned peers in this slice.",
            }
        },
        text="conflict",
    )
    response.raise_for_status.side_effect = _http_status_error(response)
    client.post.return_value = response

    result = CliRunner().invoke(main, ["peer", "restart", "proj-claude-code"])

    assert result.exit_code != 0
    assert "Failed to restart peer" in result.output
    assert "unsupported_pane_ownership" in result.output


def test_session_resume_posts_request_and_prints_response(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200,
        {
            "ok": True,
            "repowire_session_id": "rw-session-abc123",
            "session_status": "resumable",
            "status": "resume_available",
            "capability": "supported",
            "message": "Backend resume spawned for this runtime session.",
            "backend": "codex",
            "runtime_session_id": "codex-runtime-1",
            "action": "spawned",
            "spawned_display_name": "repo-codex",
            "tmux_session": "0:repo",
            "pane_id": "%99",
        },
    )

    result = CliRunner().invoke(
        main,
        [
            "session",
            "resume",
            "rw-session-abc123",
            "--profile",
            "fast",
            "--message",
            "continue",
            "--from-peer",
            "dashboard",
        ],
    )

    assert result.exit_code == 0, result.output
    assert client.post.call_args.args[0].endswith(
        "/sessions/rw-session-abc123/controls/resume"
    )
    assert client.post.call_args.kwargs["json"] == {
        "dry_run": False,
        "profile": "fast",
        "message": "continue",
        "from_peer": "dashboard",
    }
    assert "Resumed session" in result.output
    assert "repo-codex" in result.output
    assert "%99" in result.output


def test_session_resume_dry_run_json(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(
        200,
        {
            "ok": True,
            "repowire_session_id": "rw-session-abc123",
            "session_status": "resumable",
            "status": "resume_available",
            "capability": "supported",
            "message": "Backend resume is available for this runtime session.",
            "backend": "codex",
            "runtime_session_id": "codex-runtime-1",
            "action": "inspect",
        },
    )

    result = CliRunner().invoke(
        main,
        ["session", "resume", "rw-session-abc123", "--dry-run", "--json"],
    )

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["json"] == {"dry_run": True}
    assert '"action": "inspect"' in result.output


# --- peer asks -----------------------------------------------------------------


def test_asks_uses_explicit_peer_id(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.get.return_value = _response(
        200,
        {"asks": [{
            "correlation_id": "ask-abc",
            "from_peer": "claude",
            "to_peer": "agy-demo",
            "text": "ping",
            "created_at": "2026-05-23T10:00:00Z",
            "direction": "inbound",
        }]},
    )

    result = CliRunner().invoke(main, ["peer", "asks", "--peer-id", "p-1"])

    assert result.exit_code == 0, result.output
    assert client.get.call_args.kwargs["params"] == {"peer_id": "p-1", "direction": "inbound"}
    assert "ask-abc" in result.output
    assert "claude" in result.output


def test_asks_uses_config_auth_token(monkeypatch) -> None:
    _set_auth_token(monkeypatch)
    client = _make_client(monkeypatch)
    client.get.return_value = _response(200, {"asks": []})

    result = CliRunner().invoke(main, ["peer", "asks", "--peer-id", "p-1"])

    assert result.exit_code == 0, result.output
    assert client.get.call_args.kwargs["headers"] == {"Authorization": "Bearer secret-token"}


def test_asks_resolves_pane_to_peer_id(monkeypatch) -> None:
    monkeypatch.setenv("TMUX_PANE", "%7")
    client = _make_client(monkeypatch)
    client.get.side_effect = [
        _response(200, {"peer_id": "p-7", "display_name": "agy-demo"}),
        _response(200, {"asks": []}),
    ]

    result = CliRunner().invoke(main, ["peer", "asks"])

    assert result.exit_code == 0, result.output
    assert client.get.call_args_list[0].args[0].endswith("/peers/by-pane/%257")
    assert client.get.call_args_list[1].kwargs["params"]["peer_id"] == "p-7"


def test_asks_no_identity_exits_nonzero(monkeypatch) -> None:
    monkeypatch.delenv("TMUX_PANE", raising=False)
    _make_client(monkeypatch)

    result = CliRunner().invoke(main, ["peer", "asks"])

    assert result.exit_code == 1
    assert "No peer identity" in result.output


def test_asks_json_output(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.get.return_value = _response(200, {"asks": [{"correlation_id": "x"}]})

    result = CliRunner().invoke(main, ["peer", "asks", "--peer-id", "p-1", "--json"])

    assert result.exit_code == 0, result.output
    assert '"correlation_id"' in result.output


# --- peer ack ------------------------------------------------------------------


def test_ack_bare(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(200, {"ok": True})

    result = CliRunner().invoke(main, ["peer", "ack", "ask-abc"])

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["json"] == {"correlation_id": "ask-abc"}
    assert "Acked #ask-abc" in result.output


def test_ack_with_message_and_from_peer(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(200, {"ok": True})

    result = CliRunner().invoke(
        main, ["peer", "ack", "ask-abc", "-m", "pong", "--from-peer", "agy-demo"],
    )

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["json"] == {
        "correlation_id": "ask-abc",
        "message": "pong",
        "from_peer": "agy-demo",
    }
    assert "with reply" in result.output


def test_ack_uses_config_auth_token(monkeypatch) -> None:
    _set_auth_token(monkeypatch)
    client = _make_client(monkeypatch)
    client.post.return_value = _response(200, {"ok": True})

    result = CliRunner().invoke(main, ["peer", "ack", "ask-abc"])

    assert result.exit_code == 0, result.output
    assert client.post.call_args.kwargs["headers"] == {"Authorization": "Bearer secret-token"}


def test_ack_404(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(404, text="not found")

    result = CliRunner().invoke(main, ["peer", "ack", "missing"])

    assert result.exit_code == 1
    assert "No open ask" in result.output


def test_ack_410_already_closed(monkeypatch) -> None:
    client = _make_client(monkeypatch)
    client.post.return_value = _response(410, text="closed")

    result = CliRunner().invoke(main, ["peer", "ack", "old", "-m", "late"])

    assert result.exit_code == 1
    assert "already closed" in result.output
    assert "Send a new notify/ask instead" in result.output
