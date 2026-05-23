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
