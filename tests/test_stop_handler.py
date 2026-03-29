"""Tests for the stop hook handler."""

import json
from pathlib import Path
from unittest.mock import patch

from repowire.hooks.stop_handler import main


def _make_transcript(tmp_path: Path, entries: list[dict]) -> Path:
    """Write a fake transcript JSONL file."""
    tp = tmp_path / "transcript.jsonl"
    with open(tp, "w") as f:
        for entry in entries:
            f.write(json.dumps(entry) + "\n")
    return tp


def _run_hook(input_data: dict) -> int:
    """Run the stop hook with given input data."""
    with patch("sys.stdin") as mock_stdin:
        mock_stdin.read.return_value = json.dumps(input_data)
        return main()


class TestStopHandler:
    def test_returns_zero_on_invalid_json(self):
        with patch("sys.stdin") as mock_stdin:
            mock_stdin.read.return_value = "not json"
            assert main() == 0

    def test_returns_zero_when_stop_hook_active(self):
        result = _run_hook({"stop_hook_active": True})
        assert result == 0

    def test_returns_zero_without_transcript(self):
        with patch("repowire.hooks.stop_handler.get_pane_id", return_value=None), \
             patch("repowire.hooks.stop_handler.update_status", return_value=True):
            result = _run_hook({
                "cwd": "/tmp/test",
                "session_id": "abc12345-rest",
            })
            assert result == 0

    @patch("repowire.hooks.stop_handler.daemon_post")
    @patch("repowire.hooks.stop_handler.update_status", return_value=True)
    @patch("repowire.hooks.stop_handler.get_pane_id", return_value="%42")
    def test_posts_chat_turns(self, mock_pane, mock_status, mock_post, tmp_path):
        tp = _make_transcript(tmp_path, [
            {"type": "user", "message": {"content": "Fix the bug"}},
            {"type": "assistant", "message": {"content": [
                {"type": "text", "text": "Fixed!"},
            ]}},
        ])
        _run_hook({
            "cwd": str(tmp_path),
            "session_id": "abc12345-rest",
            "transcript_path": str(tp),
        })

        # Should post user turn, assistant turn, and response
        calls = mock_post.call_args_list
        paths = [c[0][0] for c in calls]
        assert "/events/chat" in paths
        assert "/response" in paths

    @patch("repowire.hooks.stop_handler.daemon_post")
    @patch("repowire.hooks.stop_handler.update_status", return_value=True)
    @patch("repowire.hooks.stop_handler.get_pane_id", return_value="%42")
    def test_uses_session_id_as_peer_name(self, mock_pane, mock_status, mock_post, tmp_path):
        tp = _make_transcript(tmp_path, [
            {"type": "user", "message": {"content": "Hi"}},
            {"type": "assistant", "message": {"content": [
                {"type": "text", "text": "Hello!"},
            ]}},
        ])
        _run_hook({
            "cwd": str(tmp_path),
            "session_id": "abc12345-rest-of-id",
            "transcript_path": str(tp),
        })

        # peer name should be first 8 chars of session_id
        chat_calls = [c for c in mock_post.call_args_list if c[0][0] == "/events/chat"]
        assert len(chat_calls) >= 1
        payload = chat_calls[0][0][1]
        assert payload["peer"] == "abc12345"

    @patch("repowire.hooks.stop_handler.daemon_post")
    @patch("repowire.hooks.stop_handler.update_status", return_value=True)
    @patch("repowire.hooks.stop_handler.get_pane_id", return_value="%42")
    def test_includes_tool_calls(self, mock_pane, mock_status, mock_post, tmp_path):
        tp = _make_transcript(tmp_path, [
            {"type": "user", "message": {"content": "Run tests"}},
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "name": "Bash", "input": {"command": "pytest"}},
            ]}},
            {"type": "user", "message": {"content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": "passed"},
            ]}},
            {"type": "assistant", "message": {"content": [
                {"type": "text", "text": "Tests passed!"},
            ]}},
        ])
        _run_hook({
            "cwd": str(tmp_path),
            "session_id": "abc12345-rest",
            "transcript_path": str(tp),
        })

        # Find assistant chat_turn
        chat_calls = [c for c in mock_post.call_args_list if c[0][0] == "/events/chat"]
        assistant_calls = [c for c in chat_calls if c[0][1].get("role") == "assistant"]
        assert len(assistant_calls) == 1
        payload = assistant_calls[0][0][1]
        assert payload["tool_calls"] is not None
        assert len(payload["tool_calls"]) == 1
        assert payload["tool_calls"][0]["name"] == "Bash"
        assert "pytest" in payload["tool_calls"][0]["input"]

    @patch("repowire.hooks.stop_handler.daemon_post")
    @patch("repowire.hooks.stop_handler.update_status", return_value=True)
    @patch("repowire.hooks.stop_handler.get_pane_id", return_value="%42")
    def test_gemini_after_agent_with_final_response(self, mock_pane, mock_status, mock_post):
        """Test Gemini's AfterAgent hook which provides final_response but no transcript_path."""
        _run_hook({
            "hook_event_name": "AfterAgent",
            "cwd": "/tmp/test",
            "session_id": "gemini123-rest",
            "final_response": "I am finished.",
        })

        # Should update status
        mock_status.assert_called_once_with("%42", "online", use_pane_id=True)

        # Should post assistant turn for dashboard
        chat_calls = [c for c in mock_post.call_args_list if c[0][0] == "/events/chat"]
        assert len(chat_calls) == 1
        payload = chat_calls[0][0][1]
        assert payload["peer"] == "gemini12"
        assert payload["role"] == "assistant"
        assert payload["text"] == "I am finished."

        # Should post response for query resolution
        response_calls = [c for c in mock_post.call_args_list if c[0][0] == "/response"]
        assert len(response_calls) == 1
        payload = response_calls[0][0][1]
        assert payload["pane_id"] == "%42"
        assert payload["text"] == "I am finished."
