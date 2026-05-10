"""Tests for hooks/ask_lifecycle scanner — ack/reply detection in transcripts."""

import json
from pathlib import Path

from repowire.hooks.ask_lifecycle import (
    _scan_acks_and_replies,
    format_reminder_block,
)


def _write_transcript(tmp_path: Path, entries: list[dict]) -> Path:
    p = tmp_path / "t.jsonl"
    p.write_text("\n".join(json.dumps(e) for e in entries))
    return p


def _assistant_with_tool(name: str, tool_input: dict) -> dict:
    return {
        "type": "assistant",
        "message": {
            "content": [
                {"type": "tool_use", "name": name, "input": tool_input},
            ],
        },
    }


def _user(text: str = "go") -> dict:
    return {"type": "user", "message": {"content": text}}


class TestScanAcksAndReplies:
    def test_empty_when_no_path(self):
        acked, replied = _scan_acks_and_replies(None)
        assert acked == set()
        assert replied == set()

    def test_detects_ack_tool_call(self, tmp_path):
        path = _write_transcript(tmp_path, [
            _user(),
            _assistant_with_tool("ack", {"correlation_id": "ask-aaa"}),
        ])
        acked, replied = _scan_acks_and_replies(path)
        assert acked == {"ask-aaa"}
        assert replied == set()

    def test_detects_namespaced_ack(self, tmp_path):
        path = _write_transcript(tmp_path, [
            _user(),
            _assistant_with_tool("mcp__repowire__ack", {"correlation_id": "ask-bbb"}),
        ])
        acked, _ = _scan_acks_and_replies(path)
        assert acked == {"ask-bbb"}

    def test_detects_reply_to_in_ask(self, tmp_path):
        path = _write_transcript(tmp_path, [
            _user(),
            _assistant_with_tool("ask", {
                "peer_name": "x", "query": "follow-up", "reply_to": "ask-prior",
            }),
        ])
        acked, replied = _scan_acks_and_replies(path)
        assert acked == set()
        assert replied == {"ask-prior"}

    def test_ask_without_reply_to_ignored(self, tmp_path):
        path = _write_transcript(tmp_path, [
            _user(),
            _assistant_with_tool("ask", {"peer_name": "x", "query": "fresh"}),
        ])
        _, replied = _scan_acks_and_replies(path)
        assert replied == set()

    def test_only_last_turn(self, tmp_path):
        """An ack from a prior turn shouldn't bleed into the current scan."""
        path = _write_transcript(tmp_path, [
            _user(),
            _assistant_with_tool("ack", {"correlation_id": "old-cid"}),
            _user("new prompt"),
            _assistant_with_tool("ack", {"correlation_id": "new-cid"}),
        ])
        acked, _ = _scan_acks_and_replies(path)
        assert acked == {"new-cid"}
        assert "old-cid" not in acked

    def test_multiple_acks_in_one_turn(self, tmp_path):
        path = _write_transcript(tmp_path, [
            _user(),
            {
                "type": "assistant",
                "message": {
                    "content": [
                        {"type": "tool_use", "name": "ack",
                         "input": {"correlation_id": "ask-1"}},
                        {"type": "tool_use", "name": "ack",
                         "input": {"correlation_id": "ask-2"}},
                    ],
                },
            },
        ])
        acked, _ = _scan_acks_and_replies(path)
        assert acked == {"ask-1", "ask-2"}

    def test_tool_results_dont_break_walk(self, tmp_path):
        """tool_result entries (type=user) shouldn't terminate the backward walk."""
        path = _write_transcript(tmp_path, [
            _user("real prompt"),
            _assistant_with_tool("ack", {"correlation_id": "ask-x"}),
            {
                "type": "user",
                "message": {
                    "content": [
                        {"type": "tool_result", "tool_use_id": "abc", "content": "ok"},
                    ],
                },
            },
            _assistant_with_tool("ack", {"correlation_id": "ask-y"}),
        ])
        acked, _ = _scan_acks_and_replies(path)
        # Both assistants in the same turn (separated by tool_result) collected
        assert acked == {"ask-x", "ask-y"}


class TestFormatReminderBlock:
    def test_empty(self):
        assert format_reminder_block([]) == ""

    def test_single_includes_snippet(self):
        """Single ask: cid + asker + body snippet for compaction recovery."""
        block = format_reminder_block([{
            "correlation_id": "ask-x", "from_peer": "alice", "text": "what's the status?",
        }])
        assert "ask-x" in block
        assert "@alice" in block
        assert "what's the status?" in block  # body included as snippet
        assert "ack(corr_id)" in block

    def test_multiple_lists_each(self):
        block = format_reminder_block([
            {"correlation_id": "ask-x", "from_peer": "alice", "text": "first"},
            {"correlation_id": "ask-y", "from_peer": "bob", "text": "second"},
        ])
        assert "ask-x" in block
        assert "ask-y" in block
        assert "@alice" in block
        assert "@bob" in block
        assert "first" in block
        assert "second" in block

    def test_truncates_long_body(self):
        """Per-ask body snippet capped to bound block size under verbose asks."""
        long_text = "x" * 5000
        block = format_reminder_block([{
            "correlation_id": "ask-x", "from_peer": "a", "text": long_text,
        }])
        # Truncated, not 5000 chars
        assert len(block) < 400
        assert "…" in block

    def test_handles_empty_text(self):
        block = format_reminder_block([{
            "correlation_id": "ask-x", "from_peer": "a", "text": "",
        }])
        assert "ask-x" in block
        assert "@a" in block
