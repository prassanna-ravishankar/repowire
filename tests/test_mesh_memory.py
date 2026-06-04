"""Tests for filesystem-backed mesh memory."""

from __future__ import annotations

from pathlib import Path

import pytest
from click.testing import CliRunner

from repowire import memory
from repowire.cli import main


@pytest.fixture
def tmp_config(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    fake_config_dir = tmp_path / ".repowire"
    fake_config_dir.mkdir()
    monkeypatch.setattr(
        "repowire.config.paths.get_config_dir",
        lambda: fake_config_dir,
    )
    return fake_config_dir


def _write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def _memory_doc(name: str, body: str, memory_type: str = "project") -> str:
    return f"""---
name: {name}
description: {name} summary
metadata:
  type: {memory_type}
  updated_at: 2026-05-25
---

# {name}

{body}
"""


def test_resolve_scope_paths(tmp_config: Path) -> None:
    label, path = memory.resolve_scope_path("global")
    assert label == "global"
    assert path == tmp_config / "memory" / "global"

    label, path = memory.resolve_scope_path("project", project="repowire")
    assert label == "projects/repowire"
    assert path == tmp_config / "memory" / "projects" / "repowire"

    label, path = memory.resolve_scope_path("project")
    assert label == f"projects/{Path.cwd().name}"
    assert path == tmp_config / "memory" / "projects" / Path.cwd().name


def test_persona_scope_uses_active_persona(tmp_config: Path) -> None:
    active = tmp_config / "orchestrator" / "personas" / "ACTIVE_PERSONA"
    _write(active, "anya\n")

    label, path = memory.resolve_scope_path("persona")
    assert label == "personas/anya"
    assert path == tmp_config / "memory" / "personas" / "anya"


def test_list_read_and_search_memory(tmp_config: Path) -> None:
    _write(
        tmp_config / "memory" / "projects" / "repowire" / "routing.md",
        _memory_doc("routing", "Transport-neutral routing note."),
    )
    _write(
        tmp_config / "memory" / "projects" / "repowire" / "MEMORY.md",
        "# Index\n",
    )

    rows = memory.list_memories("project", project="repowire")
    assert [(row.slug, row.type, row.description) for row in rows] == [
        ("routing", "project", "routing summary")
    ]

    content, path = memory.read_memory("routing", "project", project="repowire")
    assert path.name == "routing.md"
    assert "Transport-neutral routing note." in content

    hits = memory.search_memories("neutral", "project", project="repowire")
    assert len(hits) == 1
    assert hits[0].scope == "projects/repowire"
    assert hits[0].slug == "routing"


def test_list_memory_tolerates_malformed_frontmatter(tmp_config: Path) -> None:
    _write(
        tmp_config / "memory" / "projects" / "repowire" / "routing.md",
        """---
name: routing
description: `git status` can include backticks when written by hand
metadata:
  type: project
---

# routing

Transport-neutral routing note.
""",
    )

    rows = memory.list_memories("project", project="repowire")

    assert [(row.slug, row.type, row.description) for row in rows] == [
        ("routing", "", "")
    ]


def test_write_memory_creates_file_and_index(tmp_config: Path) -> None:
    path = memory.write_memory(
        "operator-style",
        "**Why:** user prefers direct answers.",
        "user",
        memory_type="user",
        description="communication preference",
    )

    assert path == tmp_config / "memory" / "user" / "operator-style.md"
    content, read_path = memory.read_memory("operator-style", "user")
    assert read_path == path
    assert "operator-style" in content
    assert "**Why:** user prefers direct answers." in content

    rows = memory.list_memories("user")
    assert [(row.slug, row.type, row.description) for row in rows] == [
        ("operator-style", "user", "communication preference")
    ]
    index = tmp_config / "memory" / "user" / "MEMORY.md"
    assert "[operator-style](operator-style.md)" in index.read_text(encoding="utf-8")


def test_write_memory_requires_force_or_append(tmp_config: Path) -> None:
    memory.write_memory("handoff", "first note", "global")

    with pytest.raises(FileExistsError):
        memory.write_memory("handoff", "second note", "global")

    memory.write_memory("handoff", "second note", "global", append=True)
    content, _path = memory.read_memory("handoff", "global")
    assert "first note" in content
    assert "second note" in content

    memory.write_memory("handoff", "replacement", "global", overwrite=True)
    content, _path = memory.read_memory("handoff", "global")
    assert "replacement" in content
    assert "first note" not in content


def test_search_all_scopes(tmp_config: Path) -> None:
    _write(
        tmp_config / "memory" / "global" / "commits.md",
        _memory_doc("commits", "Use short commit messages.", "global"),
    )
    _write(
        tmp_config / "memory" / "personas" / "anya" / "voice.md",
        _memory_doc("voice", "Anya answers concisely.", "user"),
    )

    hits = memory.search_memories("answers", all_scopes=True)
    assert [(hit.scope, hit.slug) for hit in hits] == [("personas/anya", "voice")]


def test_cli_memory_list_show_search(tmp_config: Path) -> None:
    _write(
        tmp_config / "memory" / "projects" / "repowire" / "routing.md",
        _memory_doc("routing", "Transport-neutral routing note."),
    )
    runner = CliRunner()

    res = runner.invoke(main, ["memory", "list", "--scope", "project", "--project", "repowire"])
    assert res.exit_code == 0, res.output
    assert "routing" in res.output
    assert "routing summary" in res.output

    res = runner.invoke(
        main,
        ["memory", "show", "routing", "--scope", "project", "--project", "repowire"],
    )
    assert res.exit_code == 0, res.output
    assert "Transport-neutral routing note." in res.output

    res = runner.invoke(
        main,
        ["memory", "search", "neutral", "--scope", "project", "--project", "repowire"],
    )
    assert res.exit_code == 0, res.output
    assert "projects/repowire/routing" in res.output


def test_cli_memory_write_and_duplicate_refusal(tmp_config: Path) -> None:
    runner = CliRunner()

    res = runner.invoke(
        main,
        [
            "memory",
            "write",
            "operator-style",
            "--scope",
            "user",
            "--body",
            "Prefer concise handoffs.",
            "--type",
            "user",
            "--description",
            "handoff preference",
        ],
    )
    assert res.exit_code == 0, res.output
    assert "Wrote" in res.output

    res = runner.invoke(main, ["memory", "list", "--scope", "user"])
    assert res.exit_code == 0, res.output
    assert "operator-style" in res.output
    assert "handoff preference" in res.output

    res = runner.invoke(main, ["memory", "show", "operator-style", "--scope", "user"])
    assert res.exit_code == 0, res.output
    assert "Prefer concise handoffs." in res.output

    res = runner.invoke(main, ["memory", "search", "concise", "--scope", "user"])
    assert res.exit_code == 0, res.output
    assert "operator-style" in res.output

    duplicate = runner.invoke(
        main,
        ["memory", "write", "operator-style", "--scope", "user", "--body", "again"],
    )
    assert duplicate.exit_code != 0
    assert "memory already exists" in duplicate.output
