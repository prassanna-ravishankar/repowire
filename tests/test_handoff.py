"""Tests for hook-level session handoff summaries."""

import json
from pathlib import Path

from repowire.hooks.handoff import (
    HANDOFF_WORD_LIMIT,
    build_handoff_summary,
    load_handoff_context,
    write_handoff_summary,
)


def _make_transcript(tmp_path: Path, user_text: str, assistant_text: str) -> Path:
    path = tmp_path / "transcript.jsonl"
    path.write_text(
        "\n".join([
            json.dumps({"type": "user", "message": {"content": user_text}}),
            json.dumps({
                "type": "assistant",
                "message": {"content": [{"type": "text", "text": assistant_text}]},
            }),
        ]) + "\n",
    )
    return path


def test_summary_is_capped(tmp_path):
    transcript = _make_transcript(
        tmp_path,
        "hello",
        " ".join(f"word{i}" for i in range(HANDOFF_WORD_LIMIT + 50)),
    )

    summary = build_handoff_summary(transcript_path=transcript)

    assert summary is not None
    assert len(summary.split()) <= HANDOFF_WORD_LIMIT


def test_write_persists_summary_only_for_matching_identity(tmp_path, monkeypatch):
    monkeypatch.setattr("repowire.config.models.CACHE_DIR", tmp_path)
    transcript = _make_transcript(tmp_path, "raw user prompt", "raw assistant answer")

    write_handoff_summary(
        cwd=str(tmp_path / "repo"),
        backend="claude-code",
        session_id="session-1",
        transcript_path=transcript,
    )

    files = list((tmp_path / "handoffs").glob("*.json"))
    assert len(files) == 1
    payload = json.loads(files[0].read_text())
    assert sorted(payload) == [
        "backend",
        "cwd",
        "session_id",
        "summary",
        "updated_at",
        "word_limit",
    ]
    assert "raw user prompt" in payload["summary"]
    assert "raw assistant answer" in payload["summary"]

    assert load_handoff_context(
        cwd=str(tmp_path / "repo"),
        backend="claude-code",
        session_id="session-1",
    ).startswith("[Repowire Session Handoff]")
    assert load_handoff_context(
        cwd=str(tmp_path / "repo"),
        backend="claude-code",
        session_id="different-session",
    ) is None
