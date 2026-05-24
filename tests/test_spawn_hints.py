"""Tests for spawn_hints module."""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from repowire import spawn_hints
from repowire.spawn_hints import consume_hint, consume_hint_full, write_hint


@pytest.fixture
def tmp_cache(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Redirect spawn_hints to a temp cache dir."""
    monkeypatch.setattr("repowire.spawn_hints.CACHE_DIR", tmp_path)
    return tmp_path


def test_write_then_consume_returns_circle(tmp_cache: Path) -> None:
    write_hint("/tmp/proj", "codex", "5")
    assert consume_hint("/tmp/proj", "codex") == "5"


def test_consume_deletes_hint(tmp_cache: Path) -> None:
    write_hint("/tmp/proj", "codex", "5")
    consume_hint("/tmp/proj", "codex")
    assert consume_hint("/tmp/proj", "codex") is None


def test_consume_missing_returns_none(tmp_cache: Path) -> None:
    assert consume_hint("/tmp/never-spawned", "codex") is None


def test_hints_keyed_by_path_and_backend(tmp_cache: Path) -> None:
    write_hint("/tmp/proj", "codex", "5")
    write_hint("/tmp/proj", "claude-code", "7")
    assert consume_hint("/tmp/proj", "codex") == "5"
    assert consume_hint("/tmp/proj", "claude-code") == "7"


def test_hint_preserves_restart_peer_id(tmp_cache: Path) -> None:
    write_hint(
        "/tmp/proj",
        "codex",
        "agentbox",
        role="orchestrator",
        peer_id="repow-agentbox-abc12345",
    )
    assert consume_hint_full("/tmp/proj", "codex") == {
        "circle": "agentbox",
        "role": "orchestrator",
        "peer_id": "repow-agentbox-abc12345",
    }


def test_hints_for_same_path_and_backend_are_fifo(tmp_cache: Path) -> None:
    write_hint("/tmp/proj", "codex", "agentbox")
    write_hint("/tmp/proj", "codex", "agentbox")
    assert consume_hint("/tmp/proj", "codex") == "agentbox"
    assert consume_hint("/tmp/proj", "codex") == "agentbox"
    assert consume_hint("/tmp/proj", "codex") is None


def test_stale_hint_returns_none_and_is_deleted(
    tmp_cache: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    write_hint("/tmp/proj", "codex", "5")
    # Fast-forward time past the TTL.
    future = time.time() + 9999
    fake_time = type("T", (), {"time": staticmethod(lambda: future)})
    monkeypatch.setattr(spawn_hints, "time", fake_time)
    assert consume_hint("/tmp/proj", "codex") is None
    # Calling again should still return None (file already deleted).
    assert consume_hint("/tmp/proj", "codex") is None


def test_stale_hint_does_not_block_later_fresh_hint(
    tmp_cache: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    now = time.time()
    fake_time = type("T", (), {"time": staticmethod(lambda: now - 9999)})
    monkeypatch.setattr(spawn_hints, "time", fake_time)
    write_hint("/tmp/proj", "codex", "stale")

    fake_time = type("T", (), {"time": staticmethod(lambda: now)})
    monkeypatch.setattr(spawn_hints, "time", fake_time)
    write_hint("/tmp/proj", "codex", "fresh")

    assert consume_hint("/tmp/proj", "codex") == "fresh"
    assert consume_hint("/tmp/proj", "codex") is None


def test_corrupt_hint_returns_none(tmp_cache: Path) -> None:
    hints_dir = tmp_cache / "spawn-hints"
    hints_dir.mkdir(parents=True, exist_ok=True)
    write_hint("/tmp/proj", "codex", "5")
    # Corrupt the file.
    target = next(hints_dir.glob("*.json"))
    target.write_text("not json{")
    assert consume_hint("/tmp/proj", "codex") is None


def test_path_resolved_so_equivalent_paths_match(tmp_cache: Path, tmp_path: Path) -> None:
    real = tmp_path / "proj"
    real.mkdir()
    write_hint(str(real), "codex", "5")
    # Same path with a redundant segment that resolve() will normalize.
    assert consume_hint(str(real / "." ), "codex") == "5"
