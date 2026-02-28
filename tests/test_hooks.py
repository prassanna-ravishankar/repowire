"""Tests for websocket_hook helper functions."""

from __future__ import annotations

from subprocess import CompletedProcess
from unittest.mock import patch

from repowire.hooks.websocket_hook import _is_pane_safe


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
