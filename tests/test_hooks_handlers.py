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

    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_sets_busy(self, mock_pane, mock_status):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0
        mock_status.assert_called_once_with("%42", "busy", use_pane_id=True)

    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_gemini_before_agent(self, mock_pane, mock_status):
        result = _run_with_input(prompt_main, {"hook_event_name": "BeforeAgent"})
        assert result == 0
        mock_status.assert_called_once_with("%42", "busy", use_pane_id=True)

    @patch("repowire.hooks.prompt_handler.update_status")
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value=None)
    def test_no_pane_id(self, mock_pane, mock_status):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0
        mock_status.assert_not_called()

    @patch("repowire.hooks.prompt_handler.update_status", return_value=False)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_status_update_failure(self, mock_pane, mock_status):
        result = _run_with_input(prompt_main, {"hook_event_name": "UserPromptSubmit"})
        assert result == 0  # returns 0 even on failure


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

    @patch("repowire.hooks.notification_handler.update_status", return_value=True)
    @patch("repowire.hooks.notification_handler.get_pane_id", return_value="%42")
    def test_sets_online_on_idle(self, mock_pane, mock_status):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "idle_prompt",
        })
        assert result == 0
        mock_status.assert_called_once_with("%42", "online", use_pane_id=True)

    @patch("repowire.hooks.notification_handler.update_status")
    @patch("repowire.hooks.notification_handler.get_pane_id", return_value=None)
    def test_no_pane_id(self, mock_pane, mock_status):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "idle_prompt",
        })
        assert result == 0
        mock_status.assert_not_called()
