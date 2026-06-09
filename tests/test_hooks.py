"""Tests for websocket_hook helper functions."""

from __future__ import annotations

import asyncio
import json
import os
import subprocess
from subprocess import CompletedProcess
from unittest.mock import AsyncMock, patch

import pytest

import repowire.hooks.websocket_hook as websocket_hook
from repowire.hooks.websocket_hook import _is_pane_safe, _tmux_send_keys


class TestHandleAskAndNotify:
    """type=ask injects framed text; type=notify is plain FYI.

    Under the simplified model the transport no longer POSTs pickup back
    to the daemon — open asks are surfaced via Stop hook reminders.
    """

    @pytest.mark.asyncio
    async def test_type_ask_injects_framed_text(self):
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "correlation_id": "ask-abc",
                    "from_peer": "alice",
                    "text": 'ping?\n↳ ack("ask-abc") or ack("ask-abc", "reply")',
                },
                "%5",
            )
        mock_send.assert_called_once()
        injected = mock_send.call_args.args[1]
        assert "@alice" in injected
        assert "[ask #ask-abc]" in injected
        assert "ping?" in injected
        assert '↳ ack("ask-abc")' in injected

    @pytest.mark.asyncio
    async def test_type_notify_injects_plain_text(self):
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {"type": "notify", "from_peer": "alice", "text": "fyi"},
                "%5",
            )
        mock_send.assert_called_once()

    @pytest.mark.asyncio
    async def test_notify_with_delivery_id_acks_injected(self):
        websocket = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "delivery_id": "notif-delivery-1",
                    "from_peer": "alice",
                    "text": "fyi",
                },
                "%5",
                websocket,
            )

        frame = json.loads(websocket.send.await_args.args[0])
        assert frame == {
            "type": "delivery_ack",
            "delivery_id": "notif-delivery-1",
            "message_type": "notify",
            "status": "injected",
        }

    @pytest.mark.asyncio
    async def test_ask_with_delivery_id_acks_injected(self):
        websocket = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "delivery_id": "ask-delivery-1",
                    "correlation_id": "ask-abc",
                    "from_peer": "alice",
                    "text": "ping?",
                },
                "%5",
                websocket,
            )

        frame = json.loads(websocket.send.await_args.args[0])
        assert frame == {
            "type": "delivery_ack",
            "delivery_id": "ask-delivery-1",
            "message_type": "ask",
            "status": "injected",
        }

    @pytest.mark.asyncio
    async def test_notify_tmux_failure_logs_drop_without_daemon_error(self, caplog):
        websocket = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=False),
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "from_peer": "alice",
                    "to_peer": "bob",
                    "text": "fyi",
                },
                "%5",
                websocket,
            )

        websocket.send.assert_not_called()
        assert "Inbound notification dropped: tmux send-keys failed" in caplog.text
        assert "pane=%5" in caplog.text
        assert "from=alice" in caplog.text
        assert "to=bob" in caplog.text

    @pytest.mark.asyncio
    async def test_notify_with_delivery_id_acks_tmux_failure(self):
        websocket = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=False),
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "delivery_id": "notif-delivery-2",
                    "from_peer": "alice",
                    "to_peer": "bob",
                    "text": "fyi",
                },
                "%5",
                websocket,
            )

        frame = json.loads(websocket.send.await_args.args[0])
        assert frame["type"] == "delivery_ack"
        assert frame["delivery_id"] == "notif-delivery-2"
        assert frame["message_type"] == "notify"
        assert frame["status"] == "failed"
        assert "Failed to send keys" in frame["detail"]

    @pytest.mark.asyncio
    async def test_ask_with_delivery_id_acks_tmux_failure(self):
        websocket = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=False),
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "delivery_id": "ask-delivery-2",
                    "correlation_id": "ask-abc",
                    "from_peer": "alice",
                    "to_peer": "bob",
                    "text": "ping?",
                },
                "%5",
                websocket,
            )

        frame = json.loads(websocket.send.await_args.args[0])
        assert frame["type"] == "delivery_ack"
        assert frame["delivery_id"] == "ask-delivery-2"
        assert frame["message_type"] == "ask"
        assert frame["status"] == "failed"
        assert "Failed to send keys" in frame["detail"]

    @pytest.mark.asyncio
    async def test_notify_pane_safety_drop_has_no_daemon_error_frame(self, caplog):
        websocket = AsyncMock()
        with patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False):
            with pytest.raises(websocket_hook.PaneUnsafeError):
                await websocket_hook.handle_message(
                    {
                        "type": "notify",
                        "from_peer": "alice",
                        "to_peer": "bob",
                        "text": "fyi",
                    },
                    "%5",
                    websocket,
                )

        websocket.send.assert_not_called()
        assert "Inbound delivery dropped: pane %5 not safe for notify injection" in caplog.text
        assert "from=alice" in caplog.text
        assert "to=bob" in caplog.text

    @pytest.mark.asyncio
    async def test_notify_with_delivery_id_acks_pane_rejection(self):
        websocket = AsyncMock()
        with patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False):
            with pytest.raises(websocket_hook.PaneUnsafeError):
                await websocket_hook.handle_message(
                    {
                        "type": "notify",
                        "delivery_id": "notif-delivery-3",
                        "from_peer": "alice",
                        "to_peer": "bob",
                        "text": "fyi",
                    },
                    "%5",
                    websocket,
                )

        frame = json.loads(websocket.send.await_args.args[0])
        assert frame["type"] == "delivery_ack"
        assert frame["delivery_id"] == "notif-delivery-3"
        assert frame["message_type"] == "notify"
        assert frame["status"] == "rejected"
        assert "not safe for injection" in frame["detail"]

    @pytest.mark.asyncio
    async def test_ask_with_delivery_id_acks_pane_rejection(self):
        websocket = AsyncMock()
        with patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False):
            with pytest.raises(websocket_hook.PaneUnsafeError):
                await websocket_hook.handle_message(
                    {
                        "type": "ask",
                        "delivery_id": "ask-delivery-3",
                        "correlation_id": "ask-abc",
                        "from_peer": "alice",
                        "to_peer": "bob",
                        "text": "ping?",
                    },
                    "%5",
                    websocket,
                )

        frame = json.loads(websocket.send.await_args_list[0].args[0])
        assert frame["type"] == "delivery_ack"
        assert frame["delivery_id"] == "ask-delivery-3"
        assert frame["message_type"] == "ask"
        assert frame["status"] == "rejected"
        assert "not safe for injection" in frame["detail"]

    @pytest.mark.asyncio
    async def test_notify_injection_includes_recipient_when_provided(self):
        """When the daemon includes `to_peer`, the injected frame labels the
        recipient so the receiver can spot misroutes at a glance (issue #136).
        """
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "from_peer": "alice",
                    "to_peer": "bob",
                    "text": "fyi",
                },
                "%5",
            )
        injected = mock_send.call_args.args[1]
        assert "@alice" in injected
        assert "bob" in injected, (
            "recipient label missing — receiver can't tell who the message "
            f"was addressed to: {injected!r}"
        )

    @pytest.mark.asyncio
    async def test_ask_injection_includes_recipient_when_provided(self):
        """Asks also carry the recipient label so cross-circle/dup-name
        misroutes are visible to the receiver.
        """
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "correlation_id": "ask-xyz",
                    "from_peer": "alice",
                    "to_peer": "bob",
                    "text": "ping?",
                },
                "%5",
            )
        injected = mock_send.call_args.args[1]
        assert "@alice" in injected
        assert "[ask #ask-xyz]" in injected
        assert "bob" in injected, (
            "recipient label missing from ask injection: " f"{injected!r}"
        )

    @pytest.mark.asyncio
    async def test_notify_injection_omits_recipient_when_absent(self):
        """Backward-compatible: frames without `to_peer` still inject cleanly.

        Older daemons (or out-of-band test fixtures) don't populate `to_peer`;
        the hook must not crash or inject an empty label.
        """
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {"type": "notify", "from_peer": "alice", "text": "fyi"},
                "%5",
            )
        injected = mock_send.call_args.args[1]
        assert "@alice" in injected
        assert "fyi" in injected
        # no stray "to:" label when to_peer missing
        assert "to:" not in injected.lower() or "to:None" not in injected

    @pytest.mark.asyncio
    async def test_ack_reply_arrives_as_plain_notify(self):
        """Ack-with-msg replies are plain notifies."""
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True) as mock_send,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "from_peer": "bob",
                    "text": "[ack #ask-original from @bob] all good",
                },
                "%5",
            )
        mock_send.assert_called_once()

    @pytest.mark.asyncio
    async def test_query_pushes_cid_to_fifo(self):
        """Legacy /query path uses the query FIFO."""
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
            patch("repowire.hooks.websocket_hook._push_query_cid") as mock_push,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "query",
                    "correlation_id": "query-abc",
                    "from_peer": "alice",
                    "text": "blocking?",
                },
                "%5",
            )
        mock_push.assert_called_once_with("%5", "query-abc")


class TestTmuxSendKeys:
    """Tests for _tmux_send_keys."""

    @staticmethod
    def _mode_result(in_mode: bool) -> CompletedProcess:
        return CompletedProcess(
            args=[], returncode=0, stdout=("1" if in_mode else "0") + "\n", stderr=""
        )

    def test_closes_bracketed_paste_without_bare_escape(self):
        """Normal mode: no -X cancel issued, just the literal/paste-close/Enter sequence."""
        with (
            patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run,
            patch("repowire.hooks.websocket_hook.time.sleep"),
        ):
            mock_run.side_effect = [
                self._mode_result(False),  # display-message (copy-mode probe)
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # -l text
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # -H close
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # Enter
            ]
            assert _tmux_send_keys("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls == [
            ["tmux", "display-message", "-t", "%5", "-p", "#{pane_in_mode}"],
            ["tmux", "send-keys", "-t", "%5", "-l", "hello"],
            ["tmux", "send-keys", "-t", "%5", "-H", "1b", "5b", "32", "30", "31", "7e"],
            ["tmux", "send-keys", "-t", "%5", "Enter"],
        ]
        assert ["tmux", "send-keys", "-t", "%5", "Escape"] not in calls
        assert ["tmux", "send-keys", "-t", "%5", "-X", "cancel"] not in calls

    def test_cancels_copy_mode_before_paste(self):
        """Copy-mode: -X cancel runs before the literal paste, in that order."""
        with (
            patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run,
            patch("repowire.hooks.websocket_hook.time.sleep"),
        ):
            mock_run.side_effect = [
                self._mode_result(True),  # display-message reports copy-mode
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # -X cancel
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # -l text
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # -H close
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),  # Enter
            ]
            assert _tmux_send_keys("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls == [
            ["tmux", "display-message", "-t", "%5", "-p", "#{pane_in_mode}"],
            ["tmux", "send-keys", "-t", "%5", "-X", "cancel"],
            ["tmux", "send-keys", "-t", "%5", "-l", "hello"],
            ["tmux", "send-keys", "-t", "%5", "-H", "1b", "5b", "32", "30", "31", "7e"],
            ["tmux", "send-keys", "-t", "%5", "Enter"],
        ]
        cancel_idx = calls.index(["tmux", "send-keys", "-t", "%5", "-X", "cancel"])
        paste_idx = calls.index(["tmux", "send-keys", "-t", "%5", "-l", "hello"])
        assert cancel_idx < paste_idx, "cancel must precede the literal paste"

    def test_mode_probe_failure_treated_as_not_in_mode(self):
        """If display-message fails, skip cancel rather than blocking the send."""
        with (
            patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run,
            patch("repowire.hooks.websocket_hook.time.sleep"),
        ):
            mock_run.side_effect = [
                CompletedProcess(args=[], returncode=1, stdout="", stderr="no pane"),
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
                CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
            ]
            assert _tmux_send_keys("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert ["tmux", "send-keys", "-t", "%5", "-X", "cancel"] not in calls


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

    def test_subprocess_exception_is_inconclusive(self):
        """FileNotFoundError (tmux not found) says nothing about the pane:
        the verdict must be None, never False (coercing transient check
        failures to "pane dead" caused mass false demotions)."""
        with patch(
            "repowire.hooks.websocket_hook.subprocess.run",
            side_effect=FileNotFoundError,
        ):
            assert _is_pane_safe("%5") is None


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


def _ps_output(rows: list[tuple[int, int, str]]) -> str:
    """Render a list of (pid, ppid, comm) as ps -axo output."""
    return "\n".join(f"{pid} {ppid} {comm}" for pid, ppid, comm in rows) + "\n"


class TestIsPaneSafeSubtree:
    """Tests for the process-subtree-based pane safety check.

    The fix replaces foreground-command (`pane_current_command`) with a walk
    of the pane's process subtree, so transient shell-outs (`git`, `python`,
    subagents) don't false-positive as pane reuse.
    """

    @pytest.fixture(autouse=True)
    def _reset_module_state(self):
        """Avoid cross-test contamination of the cached agent PID + baseline."""
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None
        yield
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None

    def _patch_subprocess(self, pane_pid: int | None, ps_rows: list[tuple[int, int, str]]):
        """Patch subprocess.run to answer tmux pane_pid + ps invocations."""
        def fake_run(args, **_kwargs):
            if args[0] == "tmux":
                stdout = "" if pane_pid is None else f"{pane_pid}\n"
                rc = 1 if pane_pid is None else 0
                return CompletedProcess(args=args, returncode=rc, stdout=stdout, stderr="")
            if args[0] == "ps":
                return CompletedProcess(
                    args=args, returncode=0, stdout=_ps_output(ps_rows), stderr="",
                )
            raise AssertionError(f"unexpected subprocess.run call: {args!r}")
        return patch("repowire.hooks.websocket_hook.subprocess.run", side_effect=fake_run)

    def test_subtree_with_agent_returns_true_even_when_foreground_is_git(self):
        """Agent shells out to git: foreground is git, but agent still in subtree."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (100, 1, "tmux"),
            (200, 100, "zsh"),       # pane shell
            (300, 200, "claude"),    # the agent
            (400, 300, "git"),       # transient subprocess
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is True
        assert websocket_hook._cached_agent_pid == 300

    def test_subtree_with_agent_returns_true_when_agent_is_foreground(self):
        """Steady state: agent is in subtree (no shell-out happening)."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "claude"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is True

    def test_subtree_without_agent_returns_false(self):
        """Pane has no agent in subtree (only the shell remains)."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (200, 1, "zsh"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is False

    def test_takeover_by_different_agent_returns_false(self):
        """User killed claude and started gemini in same pane: takeover."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "gemini"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is False

    def test_no_pane_pid_returns_false(self):
        """tmux returned no pane_pid: pane gone."""
        websocket_hook._expected_command = "claude"
        with self._patch_subprocess(pane_pid=None, ps_rows=[]):
            assert _is_pane_safe("%5") is False

    def test_alive_cached_pid_is_not_trusted_without_subtree_membership(self):
        """Regression for the orphan self-certification bug: a cached pid being
        alive proves nothing about it still belonging to this pane. Every check
        must walk the subtree; an agent-less pane is False even when the cached
        pid is demonstrably alive."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()  # guaranteed alive
        ps_rows = [(200, 1, "zsh")]  # but no agent in the pane subtree
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is False
        assert websocket_hook._cached_agent_pid is None

    def test_cached_pid_invalidated_on_process_lookup_error(self):
        """Stale cached PID triggers a rescan, not a permanent miss."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = 999_999_999  # almost certainly dead
        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "claude"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is True
        assert websocket_hook._cached_agent_pid == 300

    def test_truncated_comm_path_basenamed(self):
        """ps may emit `/usr/local/bin/claude`; basename should match `claude`."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (200, 1, "/bin/zsh"),
            (300, 200, "/usr/local/bin/claude"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is True

    def test_every_check_scans_subtree(self):
        """No liveness-only shortcut: every check must shell out and walk the
        pane subtree, even with a populated cache."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()
        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "claude"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows) as mock_run:
            assert _is_pane_safe("%5") is True
            assert mock_run.called
        assert websocket_hook._cached_agent_pid == 300

    def test_login_shell_dash_comm_is_still_a_shell(self):
        """Login shells report comm as "-zsh"; the dash must not make the
        shell pass as an agent (root cause of self-certifying orphans)."""
        websocket_hook._expected_command = "claude"
        ps_rows = [(200, 1, "-zsh")]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is False

    def test_find_pane_agent_pid_skips_login_shell(self):
        """Subtree agent discovery must skip the "-zsh" login shell and land
        on the real agent process."""
        ps_rows = [
            (200, 1, "-zsh"),
            (300, 200, "node"),
            (400, 300, "codex"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert websocket_hook.find_pane_agent_pid("%5") == 300

    def test_ps_failure_is_inconclusive(self):
        """A failing ps shell-out must yield None (no verdict), not False."""
        websocket_hook._expected_command = "claude"

        def fake_run(args, **_kwargs):
            if args[0] == "tmux":
                return CompletedProcess(args=args, returncode=0, stdout="200\n", stderr="")
            return CompletedProcess(args=args, returncode=1, stdout="", stderr="")

        with patch("repowire.hooks.websocket_hook.subprocess.run", side_effect=fake_run):
            assert _is_pane_safe("%5") is None

    def test_tmux_timeout_is_inconclusive_with_baseline(self):
        """tmux timing out says nothing about the pane even on the subtree path."""
        websocket_hook._expected_command = "claude"
        with patch(
            "repowire.hooks.websocket_hook.subprocess.run",
            side_effect=subprocess.TimeoutExpired(cmd="tmux", timeout=5),
        ):
            assert _is_pane_safe("%5") is None

    def test_root_pid_itself_matches_when_agent_replaced_pane_shell(self):
        """If the user ran `exec claude` from the shell, the agent IS the
        pane_pid (no shell parent). BFS must check root, not just descendants."""
        websocket_hook._expected_command = "claude"
        ps_rows = [
            (200, 1, "claude"),  # pane_pid IS the agent
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is True
        assert websocket_hook._cached_agent_pid == 200

    def test_eperm_on_cached_pid_triggers_rescan(self):
        """os.kill(cached_pid, 0) raising PermissionError means PID got reused
        by some non-agent process. Drop the cache and rescan rather than
        masking takeover by treating EPERM as alive."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = 12345  # arbitrary

        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "claude"),
        ]

        def fake_kill(_pid, _sig):
            raise PermissionError("EPERM")

        with (
            patch("repowire.hooks.websocket_hook.os.kill", side_effect=fake_kill),
            self._patch_subprocess(pane_pid=200, ps_rows=ps_rows),
        ):
            # Rescan finds the real claude in the subtree, so safe -- but
            # crucially, the cache was reset by the EPERM branch, then
            # repopulated by the rescan with the correct PID.
            assert _is_pane_safe("%5") is True
        assert websocket_hook._cached_agent_pid == 300

    def test_eperm_with_no_agent_in_subtree_returns_false(self):
        """Same EPERM path, but rescan also fails to find the agent --
        confirms the cache is cleared and takeover is detected."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = 12345

        ps_rows = [(200, 1, "zsh")]  # no agent

        def fake_kill(_pid, _sig):
            raise PermissionError("EPERM")

        with (
            patch("repowire.hooks.websocket_hook.os.kill", side_effect=fake_kill),
            self._patch_subprocess(pane_pid=200, ps_rows=ps_rows),
        ):
            assert _is_pane_safe("%5") is False
        assert websocket_hook._cached_agent_pid is None


class TestCaptureBaselineFromSubtree:
    """Startup baseline comes from `ps -axo comm` (same source as steady-state
    safety) instead of tmux `pane_current_command`. Guards against agents
    shipping as per-version binaries (Claude v2.1.138+) where tmux reports the
    version string but ps reports the agent name."""

    def test_returns_first_non_shell_descendant(self):
        capture = websocket_hook._capture_baseline_from_subtree

        # finds agent past shell parent, before transient subprocess
        assert capture(
            200, {200: [300], 300: [400]}, {200: "zsh", 300: "claude", 400: "git"},
        ) == "claude"

        # only shells: nothing to baseline against
        assert capture(200, {200: [201]}, {200: "zsh", 201: "bash"}) is None

        # exec'd-into-pane: agent IS the pane_pid, no shell parent
        assert capture(200, {}, {200: "claude"}) == "claude"

        # agent reports versioned binary name (Claude v2.1.138 case)
        assert capture(
            200, {200: [300]}, {200: "zsh", 300: "2.1.138"},
        ) == "2.1.138"

        # skips multiple shell layers (login → fish → claude)
        assert capture(
            100, {100: [200], 200: [300]}, {100: "login", 200: "fish", 300: "claude"},
        ) == "claude"


class TestPingHandlerThreshold:
    """The ping handler must tolerate transient unsafe results before exiting."""

    @pytest.fixture(autouse=True)
    def _reset_counter(self):
        websocket_hook._consecutive_ping_unsafe = 0
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None
        yield
        websocket_hook._consecutive_ping_unsafe = 0
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None

    @pytest.mark.asyncio
    async def test_single_unsafe_ping_does_not_raise(self):
        ws = AsyncMock()
        ws.send = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
        ):
            await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
        assert websocket_hook._consecutive_ping_unsafe == 1

    @pytest.mark.asyncio
    async def test_threshold_unsafe_pings_raise(self):
        ws = AsyncMock()
        ws.send = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
        ):
            for _ in range(websocket_hook.PANE_UNSAFE_STRIKE_LIMIT - 1):
                await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
            with pytest.raises(websocket_hook.PaneUnsafeError):
                await websocket_hook.handle_message({"type": "ping"}, "%5", ws)

    @pytest.mark.asyncio
    async def test_safe_ping_resets_counter(self):
        """A successful safety check between failures must reset the counter."""
        ws = AsyncMock()
        ws.send = AsyncMock()
        with patch(
            "repowire.hooks.websocket_hook.get_tmux_info",
            return_value={"session_name": "0"},
        ):
            with patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False):
                for _ in range(websocket_hook.PANE_UNSAFE_STRIKE_LIMIT - 1):
                    await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
            assert websocket_hook._consecutive_ping_unsafe == (
                websocket_hook.PANE_UNSAFE_STRIKE_LIMIT - 1
            )
            with patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True):
                await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
            assert websocket_hook._consecutive_ping_unsafe == 0

    @pytest.mark.asyncio
    async def test_pong_always_sent_with_pane_alive_field(self):
        """Even on unsafe, the pong must go out so the daemon knows the state."""
        ws = AsyncMock()
        ws.send = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=False),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
        ):
            await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
        ws.send.assert_called_once()
        import json as _json
        sent = _json.loads(ws.send.call_args.args[0])
        assert sent["type"] == "pong"
        assert sent["pane_alive"] is False
