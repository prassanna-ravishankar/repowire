"""Tests for websocket_hook helper functions."""

from __future__ import annotations

import asyncio
import os
from subprocess import CompletedProcess
from unittest.mock import AsyncMock, patch

import pytest

import repowire.hooks.websocket_hook as websocket_hook
from repowire.hooks.websocket_hook import _is_pane_safe, _tmux_send_keys


class TestTmuxSendKeys:
    """Tests for _tmux_send_keys."""

    def test_closes_bracketed_paste_without_bare_escape(self):
        with (
            patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run,
            patch("repowire.hooks.websocket_hook.time.sleep"),
        ):
            assert _tmux_send_keys("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls == [
            ["tmux", "send-keys", "-t", "%5", "-l", "hello"],
            ["tmux", "send-keys", "-t", "%5", "-H", "1b", "5b", "32", "30", "31", "7e"],
            ["tmux", "send-keys", "-t", "%5", "Enter"],
        ]
        assert ["tmux", "send-keys", "-t", "%5", "Escape"] not in calls


class TestIsPaneSafe:
    """Tests for _is_pane_safe."""

    def _run(self, stdout: str, returncode: int = 0) -> CompletedProcess:
        return CompletedProcess(args=[], returncode=returncode, stdout=stdout, stderr="")

    def test_empty_stdout_returns_false(self):
        """tmux exits 0 with empty stdout for non-existent panes — must return False."""
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            mock_run.return_value = self._run("")
            assert _is_pane_safe("%5") is False

    def test_shell_cmd_returns_false(self):
        """Pane running a bare shell should return False."""
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            for shell in ("bash", "zsh", "sh", "fish"):
                mock_run.return_value = self._run(shell)
                assert _is_pane_safe("%5") is False, f"Expected False for shell '{shell}'"

    def test_agent_cmd_returns_true(self):
        """Pane running an agent binary should return True."""
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            mock_run.return_value = self._run("claude")
            assert _is_pane_safe("%5") is True

    def test_version_string_returns_true(self):
        """Agent may report version string as pane_current_command — should return True."""
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            mock_run.return_value = self._run("2.1.45")
            assert _is_pane_safe("%5") is True

    def test_nonzero_exit_returns_false(self):
        """Nonzero returncode from tmux means pane is gone."""
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            mock_run.return_value = self._run("claude", returncode=1)
            assert _is_pane_safe("%5") is False

    def test_subprocess_exception_returns_false(self):
        """FileNotFoundError (tmux not found) should return False."""
        with patch(
            "repowire.hooks.websocket_hook.subprocess.run",
            side_effect=FileNotFoundError,
        ):
            assert _is_pane_safe("%5") is False


class TestWebsocketReconnect:
    @pytest.mark.asyncio
    async def test_main_keeps_retrying_after_warning_threshold(self):
        """ws-hook should keep retrying after long disconnects instead of exiting."""
        sleep_calls: list[int] = []

        async def fake_sleep(delay: int) -> None:
            sleep_calls.append(delay)
            if len(sleep_calls) >= 51:
                raise asyncio.CancelledError()

        with (
            patch.dict(os.environ, {"TMUX_PANE": "%5"}, clear=False),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
            patch("repowire.hooks.websocket_hook.get_display_name", return_value="repowire"),
            patch("repowire.hooks.websocket_hook._get_pane_command", return_value="claude"),
            patch("repowire.hooks.websocket_hook.websockets.connect", side_effect=OSError("down")),
            patch(
                "repowire.hooks.websocket_hook.asyncio.sleep",
                new_callable=AsyncMock,
            ) as mock_sleep,
            patch.object(websocket_hook.logger, "error") as mock_error,
        ):
            mock_sleep.side_effect = fake_sleep

            with pytest.raises(asyncio.CancelledError):
                await websocket_hook.main()

        assert len(sleep_calls) == 51
        assert max(sleep_calls) == websocket_hook._MAX_RECONNECT_DELAY_SECONDS
        assert any(
            "still retrying" in call.args[0]
            for call in mock_error.call_args_list
        )
