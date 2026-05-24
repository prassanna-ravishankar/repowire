"""Tests for repowire.orchestrator.persona."""

from __future__ import annotations

from pathlib import Path

import pytest
from click.testing import CliRunner

from repowire.cli import main
from repowire.orchestrator import persona


@pytest.fixture
def tmp_config(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    fake_config_dir = tmp_path / ".repowire"
    fake_config_dir.mkdir()
    monkeypatch.setattr(
        "repowire.config.models.Config.get_config_dir",
        classmethod(lambda cls: fake_config_dir),
    )
    return fake_config_dir


def _write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def test_validate_persona_name_accepts_safe_chars() -> None:
    assert persona.validate_persona_name("anya") == "anya"
    assert persona.validate_persona_name("Anya-2.0_test") == "Anya-2.0_test"


@pytest.mark.parametrize("bad", ["", "../etc", "a/b", "anya!", " "])
def test_validate_persona_name_rejects_unsafe(bad: str) -> None:
    with pytest.raises(ValueError):
        persona.validate_persona_name(bad)


def test_find_soul_prefers_workspace_over_global(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "global")
    _write(
        tmp_config / "orchestrator" / "personas" / "anya" / "SOUL.md",
        "workspace",
    )
    path, source = persona.find_soul_path("anya")
    assert source == "workspace"
    assert path is not None and path.read_text(encoding="utf-8") == "workspace"


def test_find_soul_falls_back_to_global(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "global")
    path, source = persona.find_soul_path("anya")
    assert source == "global"
    assert path is not None


def test_find_soul_returns_none_when_missing(tmp_config: Path) -> None:
    path, source = persona.find_soul_path("ghost")
    assert path is None and source is None


def test_load_soul_hashes_content(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "I am Anya.\n")
    soul = persona.load_soul("anya")
    assert soul is not None
    assert soul.name == "anya"
    assert soul.source == "global"
    assert len(soul.sha256) == 64
    assert soul.short_hash == soul.sha256[:12]


def test_set_and_clear_active_persona(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "soul")
    assert persona.get_active_persona() is None
    persona.set_active_persona("anya")
    assert persona.get_active_persona() == "anya"
    active = persona.load_active_soul()
    assert active is not None and active.name == "anya"
    assert persona.clear_active_persona() is True
    assert persona.get_active_persona() is None
    assert persona.clear_active_persona() is False


def test_set_active_persona_rejects_missing(tmp_config: Path) -> None:
    with pytest.raises(FileNotFoundError):
        persona.set_active_persona("ghost")


def test_list_personas_dedupes_and_marks_active(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "global")
    _write(tmp_config / "personas" / "scout" / "SOUL.md", "scout")
    _write(
        tmp_config / "orchestrator" / "personas" / "anya" / "SOUL.md",
        "workspace",
    )
    persona.set_active_persona("anya")
    listing = persona.list_personas()
    names = [(p.name, p.source) for p in listing]
    assert ("anya", "workspace") in names
    assert ("anya", "global") in names
    assert ("scout", "global") in names
    actives = {(p.name, p.source) for p in listing if p.active}
    assert actives == {("anya", "workspace")}


def test_build_soul_context_includes_precedence_and_hash(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "I am Anya.")
    soul = persona.load_soul("anya")
    assert soul is not None
    text = persona.build_soul_context(soul)
    assert "[Repowire Persona]" in text
    assert "anya" in text
    assert soul.short_hash in text
    assert "Precedence" in text
    assert "I am Anya." in text


def test_write_soul_creates_and_respects_overwrite(tmp_config: Path) -> None:
    path = persona.write_soul("anya", "first")
    assert path.read_text(encoding="utf-8") == "first"
    with pytest.raises(FileExistsError):
        persona.write_soul("anya", "second")
    persona.write_soul("anya", "second", overwrite=True)
    assert path.read_text(encoding="utf-8") == "second"


# -- CLI integration --------------------------------------------------------


def test_cli_persona_list_empty(tmp_config: Path) -> None:
    result = CliRunner().invoke(main, ["orchestrator", "persona", "list"])
    assert result.exit_code == 0
    assert "No personas found" in result.output


def test_cli_persona_use_list_show_clear(tmp_config: Path) -> None:
    _write(tmp_config / "personas" / "anya" / "SOUL.md", "I am Anya.\n")
    runner = CliRunner()

    res = runner.invoke(main, ["orchestrator", "persona", "use", "anya"])
    assert res.exit_code == 0, res.output
    assert "Active persona set to" in res.output

    res = runner.invoke(main, ["orchestrator", "persona", "list"])
    assert res.exit_code == 0
    assert "anya" in res.output
    assert "*" in res.output

    res = runner.invoke(main, ["orchestrator", "persona", "show"])
    assert res.exit_code == 0
    assert "I am Anya." in res.output

    res = runner.invoke(main, ["orchestrator", "persona", "path"])
    assert res.exit_code == 0
    assert "SOUL.md" in res.output

    res = runner.invoke(main, ["orchestrator", "persona", "clear"])
    assert res.exit_code == 0
    assert "cleared" in res.output


def test_cli_persona_use_missing_errors(tmp_config: Path) -> None:
    res = CliRunner().invoke(main, ["orchestrator", "persona", "use", "ghost"])
    assert res.exit_code == 0
    assert "No SOUL.md" in res.output
