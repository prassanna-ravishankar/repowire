"""Tests for repowire.orchestrator.workspace module."""

from __future__ import annotations

from pathlib import Path

import pytest

from repowire.orchestrator import workspace


@pytest.fixture
def tmp_workspace(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Redirect workspace_path() to a temp dir."""
    fake_config_dir = tmp_path / ".repowire"
    fake_config_dir.mkdir()
    monkeypatch.setattr(
        "repowire.config.paths.get_config_dir",
        lambda: fake_config_dir,
    )
    return fake_config_dir / "orchestrator"


def test_init_creates_workspace_with_all_files(tmp_workspace: Path) -> None:
    rendered, msg = workspace.init_workspace()
    assert rendered, msg
    assert tmp_workspace.is_dir()
    assert (tmp_workspace / "AGENTS.md").is_file()
    assert (tmp_workspace / "comms.md").is_file()
    assert (tmp_workspace / "projects.md").is_file()
    assert (tmp_workspace / "BOOTSTRAP.md").is_file()
    assert (tmp_workspace / "orchestrator.yaml.example").is_file()
    assert (tmp_workspace / "memory" / "MEMORY.md").is_file()
    for skill in workspace.SKILL_NAMES:
        assert (tmp_workspace / ".agents" / "skills" / skill / "SKILL.md").is_file()


def test_template_warns_against_local_ask_user_question(tmp_workspace: Path) -> None:
    rendered, msg = workspace.init_workspace()
    assert rendered, msg
    agents = (tmp_workspace / "AGENTS.md").read_text()
    assert "Do not use harness-local `AskUserQuestion`" in agents
    assert 'ask("telegram", ...)' in agents


def test_init_creates_symlinks_pointing_at_agents_md(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    source = tmp_workspace / "AGENTS.md"
    link = tmp_workspace / "CLAUDE.md"
    assert link.is_symlink(), "CLAUDE.md should be a symlink"
    assert link.resolve() == source.resolve()
    assert not (tmp_workspace / "CODEX.md").exists()
    assert not (tmp_workspace / "GEMINI.md").exists()


def test_init_creates_claude_skill_symlinks(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    for skill in workspace.SKILL_NAMES:
        link = tmp_workspace / ".claude" / "skills" / skill
        target = tmp_workspace / ".agents" / "skills" / skill
        assert link.is_symlink(), f"{skill} should be a Claude skill symlink"
        assert link.resolve() == target.resolve()


def test_init_backs_up_conflicting_claude_skill_directory(tmp_workspace: Path) -> None:
    conflict = tmp_workspace / ".claude" / "skills" / "coordination"
    conflict.mkdir(parents=True)
    (conflict / "custom.txt").write_text("custom")

    workspace.init_workspace()

    link = tmp_workspace / ".claude" / "skills" / "coordination"
    assert link.is_symlink()
    backups = list(link.parent.glob("coordination.bak.*"))
    assert len(backups) == 1
    assert (backups[0] / "custom.txt").read_text() == "custom"


def test_init_is_atomic_idempotent_no_force(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    # User edits a file; should NOT be clobbered by a second init
    (tmp_workspace / "comms.md").write_text("CUSTOM USER CONTENT")
    rendered, msg = workspace.init_workspace()
    assert not rendered
    assert "already installed" in msg
    assert (tmp_workspace / "comms.md").read_text() == "CUSTOM USER CONTENT"


def test_force_backs_up_existing_workspace(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / "comms.md").write_text("CUSTOM")
    rendered, msg = workspace.init_workspace(force=True)
    assert rendered, msg
    # Backup exists alongside, with the custom content
    backups = list(tmp_workspace.parent.glob("orchestrator.bak.*"))
    assert len(backups) == 1
    assert (backups[0] / "comms.md").read_text() == "CUSTOM"
    # Fresh workspace has the templated content
    assert "CUSTOM" not in (tmp_workspace / "comms.md").read_text()


def test_is_installed_detects_agents_md(tmp_workspace: Path) -> None:
    assert not workspace.is_installed()
    workspace.init_workspace()
    assert workspace.is_installed()


def test_validate_passes_after_init(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    ok, errors = workspace.validate_workspace()
    assert ok, errors
    assert errors == []


def test_validate_fails_when_workspace_missing(tmp_workspace: Path) -> None:
    ok, errors = workspace.validate_workspace()
    assert not ok
    assert any("missing" in e.lower() for e in errors)


def test_validate_fails_when_symlink_broken(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / "CLAUDE.md").unlink()
    ok, errors = workspace.validate_workspace()
    assert not ok
    assert any("CLAUDE.md" in e for e in errors)


def test_validate_fails_when_symlink_points_wrong(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / "CLAUDE.md").unlink()
    (tmp_workspace / "CLAUDE.md").symlink_to("comms.md")  # wrong target
    ok, errors = workspace.validate_workspace()
    assert not ok


def test_update_reports_identical_after_fresh_init(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    report = workspace.update_workspace()
    # All repowire-owned files should be identical to shipped
    statuses = dict(report)
    assert statuses.get("AGENTS.md") == "identical"
    assert statuses.get(".agents/skills/coordination/SKILL.md") == "identical"


def test_update_reports_differs_when_repowire_owned_edited(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / ".agents" / "skills" / "coordination" / "SKILL.md").write_text(
        "USER EDITED"
    )
    report = workspace.update_workspace()
    statuses = dict(report)
    assert statuses[".agents/skills/coordination/SKILL.md"] == "differs"


def test_update_ignores_orchestrator_owned_files(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    # User edits an orchestrator-owned file (comms.md). update should never report it.
    (tmp_workspace / "comms.md").write_text("USER OWNED")
    report = workspace.update_workspace()
    paths = [p for p, _ in report]
    assert "comms.md" not in paths
    assert "projects.md" not in paths
    assert "memory/MEMORY.md" not in paths


def test_update_reports_symlink_broken(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / "CLAUDE.md").unlink()
    report = workspace.update_workspace()
    statuses = dict(report)
    assert statuses["CLAUDE.md"] == "symlink-broken"


def test_update_reports_claude_skill_symlink_broken(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    (tmp_workspace / ".claude" / "skills" / "coordination").unlink()
    report = workspace.update_workspace()
    statuses = dict(report)
    assert statuses[".claude/skills/coordination"] == "symlink-broken"


def test_backup_workspace_raises_if_no_workspace(tmp_workspace: Path) -> None:
    with pytest.raises(FileNotFoundError):
        workspace.backup_workspace()


def test_backup_workspace_returns_path(tmp_workspace: Path) -> None:
    workspace.init_workspace()
    backup = workspace.backup_workspace()
    assert backup.is_dir()
    assert "orchestrator.bak." in backup.name
    assert (backup / "AGENTS.md").is_file()
    # Original is gone
    assert not tmp_workspace.exists()
