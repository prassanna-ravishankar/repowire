import json
import tempfile
from pathlib import Path

from repowire.session.transcript import (
    extract_last_turn_pair,
    extract_last_turn_raw_tool_calls,
    extract_last_turn_tool_calls,
)


class TestExtractLastTurnPair:
    def test_basic_pair(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Hello"}}) + "\n")
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "Hi there!"}]},
                    }
                )
                + "\n"
            )
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        assert user == "Hello"
        assert assistant == "Hi there!"
        path.unlink()

    def test_returns_last_of_each(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "First question"}}) + "\n")
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "First answer"}]},
                    }
                )
                + "\n"
            )
            f.write(json.dumps({"type": "user", "message": {"content": "Second question"}}) + "\n")
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "Second answer"}]},
                    }
                )
                + "\n"
            )
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        assert user == "Second question"
        assert assistant == "Second answer"
        path.unlink()

    def test_tool_use_only_turn_does_not_repeat_previous_text(self):
        """Stop hook firing on a pure tool-use turn must not re-emit the previous text response.

        Reproduces the duplicate chat bubble bug: stop fires after a tool-use-only
        assistant entry; extract_last_turn_pair previously returned the last text-bearing
        assistant entry (from an earlier turn), causing the dashboard to show the same
        message twice.
        """
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            # Turn 1: user asks, assistant responds with text
            f.write(json.dumps({"type": "user", "message": {"content": "Do something"}}) + "\n")
            f.write(json.dumps({"type": "assistant", "message": {"content": [
                {"type": "text", "text": "I'll do that now."}
            ]}}) + "\n")
            # Turn 2: assistant makes a tool call (no text) — stop hook fires here
            f.write(json.dumps({"type": "assistant", "message": {"content": [
                {"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}
            ]}}) + "\n")
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        # Must NOT return the previous "I'll do that now." — that was already posted
        assert assistant is None
        path.unlink()

    def test_no_user_messages(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "Response"}]},
                    }
                )
                + "\n"
            )
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        assert user is None
        assert assistant == "Response"
        path.unlink()

    def test_no_assistant_messages(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Just a prompt"}}) + "\n")
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        assert user == "Just a prompt"
        assert assistant is None
        path.unlink()

    def test_empty_file(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            path = Path(f.name)

        user, assistant = extract_last_turn_pair(path)
        assert user is None
        assert assistant is None
        path.unlink()

    def test_nonexistent_file(self):
        user, assistant = extract_last_turn_pair(Path("/nonexistent/path.jsonl"))
        assert user is None
        assert assistant is None


class TestExtractToolCalls:
    def test_extracts_tool_use(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Fix it"}}) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [
                    {"type": "tool_use", "name": "Read", "input": {"file_path": "/tmp/auth.py"}},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "user",
                "message": {"content": [
                    {"type": "tool_result", "tool_use_id": "t1", "content": "file contents"},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [
                    {"type": "text", "text": "Fixed!"},
                ]},
            }) + "\n")
            path = Path(f.name)

        calls = extract_last_turn_tool_calls(path)
        assert len(calls) == 1
        assert calls[0]["name"] == "Read"
        assert "auth.py" in calls[0]["input"]
        path.unlink()

    def test_multiple_tool_calls(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Do stuff"}}) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [
                    {"type": "tool_use", "name": "Bash", "input": {"command": "ls -la"}},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "user",
                "message": {"content": [
                    {"type": "tool_result", "tool_use_id": "t1", "content": ""},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [
                    {"type": "tool_use", "name": "Edit", "input": {"file_path": "/tmp/foo.py"}},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "user",
                "message": {"content": [
                    {"type": "tool_result", "tool_use_id": "t2", "content": ""},
                ]},
            }) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [{"type": "text", "text": "Done"}]},
            }) + "\n")
            path = Path(f.name)

        calls = extract_last_turn_tool_calls(path)
        assert len(calls) == 2
        assert calls[0]["name"] == "Bash"
        assert calls[1]["name"] == "Edit"
        path.unlink()

    def test_no_tool_calls(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Hi"}}) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [{"type": "text", "text": "Hello!"}]},
            }) + "\n")
            path = Path(f.name)

        calls = extract_last_turn_tool_calls(path)
        assert calls == []
        path.unlink()

    def test_nonexistent_file(self):
        calls = extract_last_turn_tool_calls(Path("/nonexistent.jsonl"))
        assert calls == []

    def test_bash_input_summary(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Run it"}}) + "\n")
            f.write(json.dumps({
                "type": "assistant",
                "message": {"content": [
                    {"type": "tool_use", "name": "Bash", "input": {"command": "pytest tests/ -v"}},
                ]},
            }) + "\n")
            path = Path(f.name)

        calls = extract_last_turn_tool_calls(path)
        assert calls[0]["input"] == "pytest tests/ -v"
        path.unlink()


class TestCodexTranscript:
    """Codex-format transcripts use response_item/function_call entries.

    Shape: {type:response_item, payload:{type:function_call, name, arguments}}
    where `arguments` is a JSON-encoded string.
    """

    def _write(self, entries: list[dict]) -> Path:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            for e in entries:
                f.write(json.dumps(e) + "\n")
            return Path(f.name)

    def test_extracts_function_call(self):
        path = self._write([
            {"type": "response_item", "payload": {"type": "user_message", "content": "go"}},
            {
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "ack",
                    "arguments": json.dumps({"correlation_id": "ask-xyz"}),
                },
            },
        ])
        calls = extract_last_turn_raw_tool_calls(path)
        assert len(calls) == 1
        assert calls[0]["name"] == "ack"
        assert calls[0]["input"] == {"correlation_id": "ask-xyz"}
        path.unlink()

    def test_handles_dict_arguments(self):
        path = self._write([
            {"type": "response_item", "payload": {"type": "user_message"}},
            {
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "ask",
                    "arguments": {"peer_name": "x", "query": "?", "reply_to": "ask-prior"},
                },
            },
        ])
        calls = extract_last_turn_raw_tool_calls(path)
        assert calls[0]["input"]["reply_to"] == "ask-prior"
        path.unlink()

    def test_invalid_arguments_string_falls_back_to_empty(self):
        path = self._write([
            {"type": "response_item", "payload": {"type": "user_message"}},
            {
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "ack",
                    "arguments": "not-json",
                },
            },
        ])
        calls = extract_last_turn_raw_tool_calls(path)
        assert calls[0]["input"] == {}
        path.unlink()

    def test_walks_back_to_user_message(self):
        path = self._write([
            {
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "old",
                    "arguments": "{}",
                },
            },
            {"type": "response_item", "payload": {"type": "user_message"}},
            {
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "new",
                    "arguments": "{}",
                },
            },
        ])
        calls = extract_last_turn_raw_tool_calls(path)
        assert [c["name"] for c in calls] == ["new"]
        path.unlink()

    def test_format_detection_does_not_break_claude(self):
        """Claude-shaped transcripts must still parse as Claude, not codex."""
        path = self._write([
            {"type": "user", "message": {"content": "go"}},
            {
                "type": "assistant",
                "message": {
                    "content": [
                        {"type": "tool_use", "name": "ack", "input": {"correlation_id": "ask-aaa"}},
                    ],
                },
            },
        ])
        calls = extract_last_turn_raw_tool_calls(path)
        assert calls[0]["input"] == {"correlation_id": "ask-aaa"}
        path.unlink()

    def test_codex_message_role_user_boundary(self):
        """Codex's real boundary is {payload:{type:message, role:user}}."""
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "old-call",
                    "arguments": "{}",
                },
            }) + "\n")
            f.write(json.dumps({
                "type": "response_item",
                "payload": {"type": "message", "role": "user", "content": "go"},
            }) + "\n")
            f.write(json.dumps({
                "type": "response_item",
                "payload": {"type": "message", "role": "assistant", "content": "ok"},
            }) + "\n")
            f.write(json.dumps({
                "type": "response_item",
                "payload": {
                    "type": "function_call",
                    "name": "new-call",
                    "arguments": json.dumps({"correlation_id": "ask-zzz"}),
                },
            }) + "\n")
            path = Path(f.name)

        calls = extract_last_turn_raw_tool_calls(path)
        names = [c["name"] for c in calls]
        assert names == ["new-call"]
        assert calls[0]["input"] == {"correlation_id": "ask-zzz"}
        path.unlink()
