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
        mock_status.assert_called_once_with(
            "%42", "busy", use_pane_id=True, turn_state="working",
        )

    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_gemini_before_agent(self, mock_pane, mock_status):
        result = _run_with_input(prompt_main, {"hook_event_name": "BeforeAgent"})
        assert result == 0
        mock_status.assert_called_once_with(
            "%42", "busy", use_pane_id=True, turn_state="working",
        )

    def test_stop_failure_not_handled_by_prompt(self):
        result = _run_with_input(prompt_main, {"hook_event_name": "StopFailure"})
        assert result == 0

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

    @patch("repowire.hooks.prompt_handler.subprocess.Popen")
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_streamer_not_spawned_when_flag_off(
        self, mock_pane, mock_status, mock_popen, tmp_path,
    ):
        transcript = tmp_path / "t.jsonl"
        transcript.write_text("")
        from repowire.config.models import Config

        with patch("repowire.config.models.load_config", return_value=Config()):
            result = _run_with_input(prompt_main, {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "hook-session-1",
                "transcript_path": str(transcript),
            })
        assert result == 0
        mock_popen.assert_not_called()

    @patch("repowire.hooks.prompt_handler.subprocess.Popen")
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_streamer_spawned_when_flag_on(
        self, mock_pane, mock_status, mock_popen, tmp_path,
    ):
        transcript = tmp_path / "t.jsonl"
        transcript.write_text("")
        from repowire.config.models import Config, ExperimentsConfig

        cfg = Config(experiments=ExperimentsConfig(chat_turn_streaming=True))
        with patch("repowire.config.models.load_config", return_value=cfg):
            result = _run_with_input(prompt_main, {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "hook-session-1",
                "transcript_path": str(transcript),
            })
        assert result == 0
        mock_popen.assert_called_once()
        argv = mock_popen.call_args[0][0]
        assert "repowire.hooks.chat_delta_streamer" in argv
        assert "--transcript" in argv
        assert "--pane-id" in argv
        assert "--session-id" in argv
        assert "hook-session-1" in argv

    @patch("repowire.hooks.prompt_handler.subprocess.Popen")
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_back_to_back_prompts_terminate_predecessor(
        self, mock_pane, mock_status, mock_popen, tmp_path,
    ):
        """Second prompt with a live predecessor must terminate it before spawning."""
        import os

        from repowire.config.models import Config, ExperimentsConfig
        from repowire.hooks.chat_delta_streamer import streamer_pid_path

        # Simulate a live predecessor by writing this test process's pid as
        # the streamer pid for pane %42 (it's alive).
        pid_path = streamer_pid_path("%42")
        pid_path.write_text(str(os.getpid()))
        transcript = tmp_path / "t.jsonl"
        transcript.write_text("")
        cfg = Config(experiments=ExperimentsConfig(chat_turn_streaming=True))

        # SIGTERM to our own pid would kill the test runner — patch os.kill.
        try:
            with patch("repowire.config.models.load_config", return_value=cfg), \
                 patch("repowire.hooks.chat_delta_streamer.os.kill"):
                result = _run_with_input(prompt_main, {
                    "hook_event_name": "UserPromptSubmit",
                    "transcript_path": str(transcript),
                })
            assert result == 0
            # Pidfile was removed by terminate_live_streamer.
            assert not pid_path.exists()
            # And a fresh streamer was spawned.
            mock_popen.assert_called_once()
        finally:
            if pid_path.exists():
                pid_path.unlink()

    @patch("repowire.hooks.prompt_handler.subprocess.Popen")
    @patch("repowire.hooks.prompt_handler.update_status", return_value=True)
    @patch("repowire.hooks.prompt_handler.get_pane_id", return_value="%42")
    def test_streamer_skipped_for_non_claude_backend(
        self, mock_pane, mock_status, mock_popen, tmp_path,
    ):
        transcript = tmp_path / "t.jsonl"
        transcript.write_text("")
        from repowire.config.models import Config, ExperimentsConfig

        cfg = Config(experiments=ExperimentsConfig(chat_turn_streaming=True))
        with patch("repowire.config.models.load_config", return_value=cfg):
            result = _run_with_input(
                lambda: prompt_main(backend="codex"),
                {
                    "hook_event_name": "UserPromptSubmit",
                    "transcript_path": str(transcript),
                },
            )
        assert result == 0
        mock_popen.assert_not_called()


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
        mock_status.assert_called_once_with(
            "%42", "online", use_pane_id=True, turn_state="awaiting_input",
        )

    @patch("repowire.hooks.notification_handler.update_status")
    @patch("repowire.hooks.notification_handler.get_pane_id", return_value=None)
    def test_no_pane_id(self, mock_pane, mock_status):
        result = _run_with_input(notification_main, {
            "hook_event_name": "Notification",
            "notification_type": "idle_prompt",
        })
        assert result == 0
        mock_status.assert_not_called()
