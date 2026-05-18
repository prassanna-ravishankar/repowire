"""Tests for repowire/session/history.py — full-replay parser + pagination.

Route tests for GET /peers/{name}/transcript live in
test_transcript_history_routes.py.
"""

from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import patch

from repowire.session.history import (
    Turn,
    _encode_cwd,
    discover_claude_sessions,
    load_peer_turns,
    page_turns,
)


def _write_jsonl(path: Path, entries: list[dict]) -> None:
    path.write_text("".join(json.dumps(e) + "\n" for e in entries))


def _u(ts: str, text: str) -> dict:
    return {
        "type": "user",
        "timestamp": ts,
        "sessionId": "s1",
        "message": {"content": text},
    }


def _a(ts: str, text: str, tool_uses: list[dict] | None = None) -> dict:
    content: list[dict] = []
    if text:
        content.append({"type": "text", "text": text})
    if tool_uses:
        content.extend(tool_uses)
    return {
        "type": "assistant",
        "timestamp": ts,
        "sessionId": "s1",
        "message": {"content": content},
    }


class TestEncodeCwd:
    def test_simple(self):
        assert _encode_cwd("/Users/x/dev/repo") == "-Users-x-dev-repo"


class TestDiscoverClaudeSessions:
    def test_returns_sorted_jsonls(self, tmp_path: Path):
        peer_path = "/peer/work/dir"
        projects_root = tmp_path / "projects"
        encoded = projects_root / _encode_cwd(peer_path)
        encoded.mkdir(parents=True)
        (encoded / "b.jsonl").write_text("")
        (encoded / "a.jsonl").write_text("")
        (encoded / "skip.txt").write_text("nope")

        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            found = discover_claude_sessions(peer_path)

        assert [p.name for p in found] == ["a.jsonl", "b.jsonl"]

    def test_empty_when_dir_missing(self, tmp_path: Path):
        absent = tmp_path / "absent"
        with patch("repowire.session.history._claude_projects_dir", return_value=absent):
            assert discover_claude_sessions("/some/path") == []

    def test_empty_when_no_peer_path(self):
        assert discover_claude_sessions(None) == []


class TestLoadPeerTurnsClaude:
    def _setup(self, tmp_path: Path, entries: list[dict]) -> tuple[str, Path]:
        peer_path = "/peer/work"
        projects_root = tmp_path / "projects"
        session_dir = projects_root / _encode_cwd(peer_path)
        session_dir.mkdir(parents=True)
        _write_jsonl(session_dir / "session.jsonl", entries)
        return peer_path, projects_root

    def test_basic_replay_newest_first(self, tmp_path: Path):
        peer_path, projects_root = self._setup(
            tmp_path,
            [
                _u("2026-01-01T00:00:00Z", "first q"),
                _a("2026-01-01T00:00:01Z", "first a"),
                _u("2026-01-02T00:00:00Z", "second q"),
                _a("2026-01-02T00:00:01Z", "second a"),
            ],
        )
        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            turns = load_peer_turns(peer_path, "claude-code")
        assert [t.text for t in turns] == ["second a", "second q", "first a", "first q"]
        assert turns[0].role == "assistant"
        assert turns[1].role == "user"

    def test_skips_tool_result_only_user_entries(self, tmp_path: Path):
        peer_path, projects_root = self._setup(
            tmp_path,
            [
                _u("2026-01-01T00:00:00Z", "real q"),
                _a("2026-01-01T00:00:01Z", "real a"),
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:02Z",
                    "sessionId": "s1",
                    "message": {"content": [{"type": "tool_result", "content": "ok"}]},
                },
            ],
        )
        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            turns = load_peer_turns(peer_path, "claude-code")
        assert len(turns) == 2

    def test_extracts_tool_calls_on_assistant_turn(self, tmp_path: Path):
        peer_path, projects_root = self._setup(
            tmp_path,
            [
                _a(
                    "2026-01-01T00:00:00Z",
                    "running",
                    tool_uses=[
                        {"type": "tool_use", "name": "Bash", "input": {"command": "ls -la"}}
                    ],
                )
            ],
        )
        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            turns = load_peer_turns(peer_path, "claude-code")
        assert turns[0].tool_calls == [{"name": "Bash", "input": "ls -la"}]

    def test_skips_malformed_lines(self, tmp_path: Path):
        peer_path, projects_root = self._setup(tmp_path, [_a("2026-01-01T00:00:00Z", "ok")])
        session_dir = projects_root / _encode_cwd(peer_path)
        with open(session_dir / "session.jsonl", "a") as f:
            f.write("not json\n")
            f.write("\n")
        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            turns = load_peer_turns(peer_path, "claude-code")
        assert len(turns) == 1

    def test_sort_merges_across_sessions(self, tmp_path: Path):
        peer_path = "/peer/work"
        projects_root = tmp_path / "projects"
        session_dir = projects_root / _encode_cwd(peer_path)
        session_dir.mkdir(parents=True)
        _write_jsonl(session_dir / "a.jsonl", [
            _u("2026-01-01T00:00:00Z", "old"),
        ])
        _write_jsonl(session_dir / "b.jsonl", [
            _u("2026-01-03T00:00:00Z", "new"),
        ])
        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            turns = load_peer_turns(peer_path, "claude-code")
        assert [t.text for t in turns] == ["new", "old"]


class TestLoadPeerTurnsBackends:
    def test_codex_returns_empty_in_v1(self):
        assert load_peer_turns("/any/path", "codex") == []

    def test_gemini_returns_empty(self):
        assert load_peer_turns("/any/path", "gemini") == []


def _t(ts: str) -> Turn:
    return Turn(role="user", text=ts, timestamp=ts, session_id="s", tool_calls=[])


class TestPageTurns:
    def test_no_cursor_returns_head(self):
        turns = [_t("2026-01-03"), _t("2026-01-02"), _t("2026-01-01")]
        page, nxt = page_turns(turns, limit=2, before=None)
        assert [t.text for t in page] == ["2026-01-03", "2026-01-02"]
        assert nxt == "2026-01-02"

    def test_cursor_filters_strictly_older(self):
        turns = [_t("2026-01-03"), _t("2026-01-02"), _t("2026-01-01")]
        page, nxt = page_turns(turns, limit=10, before="2026-01-03")
        assert [t.text for t in page] == ["2026-01-02", "2026-01-01"]
        assert nxt is None

    def test_last_page_has_null_cursor(self):
        turns = [_t("2026-01-02"), _t("2026-01-01")]
        page, nxt = page_turns(turns, limit=10, before=None)
        assert len(page) == 2
        assert nxt is None

    def test_empty_returns_empty(self):
        page, nxt = page_turns([], limit=5, before=None)
        assert page == []
        assert nxt is None
