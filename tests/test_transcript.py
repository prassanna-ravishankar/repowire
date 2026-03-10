import json
import tempfile
from pathlib import Path

from repowire.session.transcript import extract_last_assistant_response, extract_last_turn_pair


class TestTranscriptParser:
    def test_extract_text_content(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(json.dumps({"type": "user", "message": {"content": "Hello"}}) + "\n")
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "content": [{"type": "text", "text": "Hello! How can I help?"}]
                        },
                    }
                )
                + "\n"
            )
            path = Path(f.name)

        result = extract_last_assistant_response(path)
        assert result == "Hello! How can I help?"

        path.unlink()

    def test_extract_multiple_messages(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "First response"}]},
                    }
                )
                + "\n"
            )
            f.write(json.dumps({"type": "user", "message": {"content": "Follow up"}}) + "\n")
            f.write(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"content": [{"type": "text", "text": "Second response"}]},
                    }
                )
                + "\n"
            )
            path = Path(f.name)

        result = extract_last_assistant_response(path)
        assert result == "Second response"

        path.unlink()

    def test_extract_string_content(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(
                json.dumps({"type": "assistant", "message": {"content": "Direct string content"}})
                + "\n"
            )
            path = Path(f.name)

        result = extract_last_assistant_response(path)
        assert result == "Direct string content"

        path.unlink()

    def test_nonexistent_file(self):
        result = extract_last_assistant_response(Path("/nonexistent/path.jsonl"))
        assert result is None

    def test_empty_file(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            path = Path(f.name)

        result = extract_last_assistant_response(path)
        assert result is None

        path.unlink()

    def test_no_assistant_messages(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl", delete=False) as f:
            f.write(
                json.dumps({"type": "user", "message": {"content": "Only user messages"}}) + "\n"
            )
            path = Path(f.name)

        result = extract_last_assistant_response(path)
        assert result is None

        path.unlink()


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
