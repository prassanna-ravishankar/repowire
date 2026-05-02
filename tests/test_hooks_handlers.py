"""Tests for prompt and notification hook handlers."""

import json
from unittest.mock import patch

from repowire.hooks.notification_handler import main as notification_main
from repowire.hooks.prompt_handler import main as prompt_main


def _run_with_input(main_fn, input_data: dict) -> int:
    with patch("sys.stdin") as mock_stdin:
        mock_stdin.read.return_value = json.dumps(input_data)
        return main_fn()


# -- Prompt Handler --


class TestPromptHandler:
    def test_invalid_json(self):
        with patch("sys.stdin") as mock_stdin:
            mock_stdin.read.return_value = "not json"
            assert prompt_main() == 0

    def test_wrong_event(self):
        result = _run_with_input(prompt_main, {"hook_event_name": "SessionStart"})
        assert result == 0

    @patch("repowire.hooks.prompt_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.prompt_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_sets_busy(self, mock_pane, mock_status, mock_read, mock_write):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0
        mock_status.assert_called_once_with("%42", "busy", use_pane_id=True)
        mock_write.assert_called_once()

    @patch("repowire.hooks.prompt_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.prompt_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_gemini_before_agent(self, mock_pane, mock_status, mock_read, mock_write):
        result = _run_with_input(prompt_main, {"hook_event_name": "BeforeAgent"})
        assert result == 0
        mock_status.assert_called_once_with("%42", "busy", use_pane_id=True)
        mock_write.assert_called_once()

    @patch("repowire.hooks.prompt_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.prompt_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.prompt_handler.update_status")
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value=None)
    def test_no_pane_id(self, mock_pane, mock_status, mock_read, mock_write):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0
        mock_status.assert_not_called()
        mock_write.assert_not_called()

    @patch("repowire.hooks.prompt_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.prompt_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.prompt_handler.update_status", return_value=False)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_status_update_failure(self, mock_pane, mock_status, mock_read, mock_write):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0  # returns 0 even on failure
        mock_write.assert_called_once()


# -- Notification Handler --


class TestNotificationHandler:
    def test_invalid_json(self):
        with patch("sys.stdin") as mock_stdin:
            mock_stdin.read.return_value = "{{bad"
            assert notification_main() == 0

    def test_wrong_event(self):
        result = _run_with_input(notification_main, {"hook_event_name": "Stop"})
        assert result == 0

    def test_wrong_notification_type(self):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "something_else",
        })
        assert result == 0

    @patch("repowire.hooks.notification_handler.send_tmux_keys", return_value=True)
    @patch("repowire.hooks.notification_handler._pop_pending_notification", return_value=None)
    @patch("repowire.hooks.notification_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.notification_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.notification_handler.update_status", return_value=True)
    @patch("repowire.hooks.notification_handler.get_pane_id", return_value="%42")
    def test_sets_online_on_idle(
        self,
        mock_pane,
        mock_status,
        mock_read,
        mock_write,
        mock_pop,
        mock_send,
    ):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "idle_prompt",
        })
        assert result == 0
        mock_status.assert_called_once_with("%42", "online", use_pane_id=True)
        mock_write.assert_called_once()

    @patch("repowire.hooks.notification_handler.write_pane_runtime_metadata")
    @patch("repowire.hooks.notification_handler.read_pane_runtime_metadata", return_value={})
    @patch("repowire.hooks.notification_handler.update_status")
    @patch("repowire.hooks.notification_handler.get_pane_id", return_value=None)
    def test_no_pane_id(self, mock_pane, mock_status, mock_read, mock_write):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "idle_prompt",
        })
        assert result == 0
        mock_status.assert_not_called()
        mock_write.assert_not_called()
