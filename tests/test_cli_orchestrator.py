"""Click invocation tests for `repowire orchestrator` subcommands."""

from __future__ import annotations

from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from click.testing import CliRunner

from repowire.cli import main


@pytest.fixture
def tmp_workspace(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    fake_config_dir = tmp_path / ".repowire"
    fake_config_dir.mkdir()
    monkeypatch.setattr(
        "repowire.orchestrator.workspace.Config.get_config_dir",
        classmethod(lambda cls: fake_config_dir),
    )
    return fake_config_dir / "orchestrator"


def test_init_creates_workspace(tmp_workspace: Path) -> None:
    runner = CliRunner()
    result = runner.invoke(main, ["orchestrator", "init"])
    assert result.exit_code == 0, result.output
    assert "workspace created" in result.output.lower()
    assert (tmp_workspace / "AGENTS.md").exists()


def test_init_idempotent(tmp_workspace: Path) -> None:
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    result = runner.invoke(main, ["orchestrator", "init"])
    assert result.exit_code == 0
    assert "already installed" in result.output.lower()


def test_init_force_backs_up(tmp_workspace: Path) -> None:
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    (tmp_workspace / "comms.md").write_text("CUSTOM")
    result = runner.invoke(main, ["orchestrator", "init", "--force"])
    assert result.exit_code == 0
    backups = list(tmp_workspace.parent.glob("orchestrator.bak.*"))
    assert len(backups) == 1
    assert (backups[0] / "comms.md").read_text() == "CUSTOM"


def test_diff_says_uptodate_after_fresh_init(tmp_workspace: Path) -> None:
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    result = runner.invoke(main, ["orchestrator", "diff"])
    assert result.exit_code == 0
    assert "up to date" in result.output.lower()


def test_diff_shows_diff_after_edit(tmp_workspace: Path) -> None:
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    (tmp_workspace / "patterns" / "mesh-roundup.md").write_text("LOCALLY EDITED")
    result = runner.invoke(main, ["orchestrator", "diff"])
    assert result.exit_code == 0
    assert "differs" in result.output.lower() or "mesh-roundup" in result.output.lower()


def test_diff_refuses_when_not_initialized(tmp_workspace: Path) -> None:
    runner = CliRunner()
    result = runner.invoke(main, ["orchestrator", "diff"])
    assert "not initialized" in result.output.lower()


def test_start_preflight_fails_when_daemon_unreachable(tmp_workspace: Path) -> None:
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    # No daemon at 127.0.0.1:0 (intentional invalid port)
    with patch("repowire.cli._get_daemon_url", return_value="http://127.0.0.1:0"):
        result = runner.invoke(main, ["orchestrator", "start"])
    assert "daemon" in result.output.lower()


def test_start_runtime_choice_validation(tmp_workspace: Path) -> None:
    """--runtime should reject unknown values via click.Choice."""
    runner = CliRunner()
    result = runner.invoke(main, ["orchestrator", "start", "--runtime=banana"])
    assert result.exit_code != 0
    assert "invalid value" in result.output.lower() or "choice" in result.output.lower()


def test_start_spawns_with_role_orchestrator(
    tmp_workspace: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Verify SpawnConfig is built with role='orchestrator'."""
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])

    spawn_mock = MagicMock()
    spawn_mock.return_value = MagicMock(
        display_name="orchestrator",
        tmux_session="default:orchestrator",
        pane_id="%99",
    )
    monkeypatch.setattr("repowire.spawn.spawn_peer", spawn_mock)
    # Stub daemon health
    monkeypatch.setattr(
        "repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377"
    )
    httpx_get_mock = MagicMock()
    httpx_get_mock.return_value.__enter__.return_value.get.return_value = MagicMock(
        raise_for_status=lambda: None,
    )
    monkeypatch.setattr("httpx.Client", httpx_get_mock)
    # Force pi detection
    monkeypatch.setattr(
        "repowire.cli._detect_runtime_for_orchestrator", lambda: "pi"
    )
    monkeypatch.setattr("repowire.cli._configured_spawn_command", lambda backend: "pi")

    result = runner.invoke(main, ["orchestrator", "start"])
    assert result.exit_code == 0, result.output
    spawn_mock.assert_called_once()
    config = spawn_mock.call_args[0][0]
    assert config.role == "orchestrator"
    assert config.backend.value == "pi"
    assert config.command == "pi"


def _stub_start_deps(monkeypatch: pytest.MonkeyPatch, runtime: str) -> MagicMock:
    """Stub daemon health, runtime detection, and spawn for orchestrator start."""
    spawn_mock = MagicMock()
    spawn_mock.return_value = MagicMock(
        display_name="orchestrator",
        tmux_session="default:orchestrator",
        pane_id="%99",
    )
    monkeypatch.setattr("repowire.spawn.spawn_peer", spawn_mock)
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")
    httpx_get_mock = MagicMock()
    httpx_get_mock.return_value.__enter__.return_value.get.return_value = MagicMock(
        raise_for_status=lambda: None,
    )
    monkeypatch.setattr("httpx.Client", httpx_get_mock)
    monkeypatch.setattr("repowire.cli._detect_runtime_for_orchestrator", lambda: runtime)
    monkeypatch.setattr(
        "repowire.cli._configured_spawn_command",
        lambda backend: {
            "pi": "pi",
            "claude-code": "claude --dangerously-skip-permissions",
            "codex": "codex --dangerously-bypass-approvals-and-sandbox",
            "gemini": "gemini --yolo",
            "opencode": "opencode",
        }.get(backend),
    )
    return spawn_mock


def test_start_uses_default_command_when_yaml_omits_it(
    tmp_workspace: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """No `command` in orchestrator.yaml → fall back to built-in default."""
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    spawn_mock = _stub_start_deps(monkeypatch, "claude-code")

    result = runner.invoke(main, ["orchestrator", "start"])
    assert result.exit_code == 0, result.output
    config = spawn_mock.call_args[0][0]
    assert config.command == "claude --dangerously-skip-permissions"


def test_start_honors_command_override_from_yaml(
    tmp_workspace: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """`command:` in orchestrator.yaml overrides the built-in default."""
    runner = CliRunner()
    runner.invoke(main, ["orchestrator", "init"])
    (tmp_workspace / "orchestrator.yaml").write_text(
        'command: "claude --dangerously-skip-permissions --model opus"\n'
    )
    spawn_mock = _stub_start_deps(monkeypatch, "claude-code")

    result = runner.invoke(main, ["orchestrator", "start"])
    assert result.exit_code == 0, result.output
    config = spawn_mock.call_args[0][0]
    assert config.command == "claude --dangerously-skip-permissions --model opus"
