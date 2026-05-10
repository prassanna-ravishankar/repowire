"""Tests for websocket_hook helper functions."""

from __future__ import annotations

import asyncio
import os
from subprocess import CompletedProcess
from unittest.mock import AsyncMock, patch

import pytest

import repowire.hooks.websocket_hook as websocket_hook
from repowire.hooks.websocket_hook import _is_pane_safe, _tmux_send_keys


class TestHandleAskAndNotify:
    """type=ask must POST pickup directly (no FIFO). type=notify is plain FYI.

    Lifecycle invariant: pickup is reported transport-side at delivery time,
    not via a per-pane FIFO. Ack-with-msg replies arrive as plain notify and
    must not produce a pickup POST.
    """

    @pytest.mark.asyncio
    async def test_type_ask_posts_pickup(self):
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
            patch("repowire.hooks.websocket_hook._post_ask_picked_up") as mock_post,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "correlation_id": "ask-abc",
                    "from_peer": "alice",
                    "text": "ping?",
                },
                "%5",
            )
        mock_post.assert_called_once_with("%5", "ask-abc")

    @pytest.mark.asyncio
    async def test_type_notify_does_not_post_pickup(self):
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
            patch("repowire.hooks.websocket_hook._post_ask_picked_up") as mock_post,
        ):
            await websocket_hook.handle_message(
                {"type": "notify", "from_peer": "alice", "text": "fyi"},
                "%5",
            )
        mock_post.assert_not_called()

    @pytest.mark.asyncio
    async def test_ack_reply_arrives_as_plain_notify(self):
        """Ack-with-msg replies are plain notifies, no lifecycle, no pickup."""
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
            patch("repowire.hooks.websocket_hook._post_ask_picked_up") as mock_post,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "notify",
                    "from_peer": "bob",
                    "text": "[ack #ask-original from @bob] all good",
                },
                "%5",
            )
        mock_post.assert_not_called()

    @pytest.mark.asyncio
    async def test_query_does_not_post_ask_pickup(self):
        """Legacy /query path uses the query FIFO, not the ask pickup endpoint."""
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=True),
            patch("repowire.hooks.websocket_hook._tmux_send_keys", return_value=True),
            patch("repowire.hooks.websocket_hook._push_query_cid") as mock_push,
            patch("repowire.hooks.websocket_hook._post_ask_picked_up") as mock_post,
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
        mock_post.assert_not_called()


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
        websocket_hook._safety_check_count = 0
        yield
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None
        websocket_hook._safety_check_count = 0

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

    def test_cached_pid_fast_path(self):
        """When the cache is populated, no subprocesses should fire."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()  # guaranteed alive
        with patch("repowire.hooks.websocket_hook.subprocess.run") as mock_run:
            assert _is_pane_safe("%5") is True
        mock_run.assert_not_called()

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

    def test_cached_pid_periodically_rescans(self):
        """PID-reuse defense: every Nth call must do a full subtree rescan
        even when os.kill says the cached PID is alive."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()
        # Drive _safety_check_count to one short of the rescan threshold.
        websocket_hook._safety_check_count = websocket_hook._FAST_PATH_RESCAN_EVERY - 1
        ps_rows = [
            (200, 1, "zsh"),
            (300, 200, "claude"),
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows) as mock_run:
            # The next call hits the rescan cadence -> ps must fire.
            assert _is_pane_safe("%5") is True
            assert mock_run.called

    def test_periodic_rescan_detects_takeover_under_alive_pid(self):
        """If the cached PID is alive but the pane subtree no longer contains
        the agent (PID reuse), the cadence rescan must catch it."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()
        websocket_hook._safety_check_count = websocket_hook._FAST_PATH_RESCAN_EVERY - 1
        ps_rows = [
            (200, 1, "zsh"),
            # No claude in the subtree any more.
        ]
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows):
            assert _is_pane_safe("%5") is False

    def test_rescan_miss_clears_cache_so_future_checks_dont_trust_stale_pid(self):
        """Once a rescan finds no agent in the subtree, the cache must be
        cleared so the next 29 fast-path checks don't fall back to trusting
        an alive-but-unrelated PID (PID reuse defense)."""
        websocket_hook._expected_command = "claude"
        websocket_hook._cached_agent_pid = os.getpid()
        websocket_hook._safety_check_count = websocket_hook._FAST_PATH_RESCAN_EVERY - 1
        ps_rows_takeover = [(200, 1, "zsh")]  # no agent in subtree
        with self._patch_subprocess(pane_pid=200, ps_rows=ps_rows_takeover):
            assert _is_pane_safe("%5") is False
        assert websocket_hook._cached_agent_pid is None

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
        websocket_hook._safety_check_count = 0
        yield
        websocket_hook._consecutive_ping_unsafe = 0
        websocket_hook._cached_agent_pid = None
        websocket_hook._expected_command = None
        websocket_hook._safety_check_count = 0

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
            for _ in range(websocket_hook._CONSECUTIVE_PANE_UNSAFE_PINGS - 1):
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
                for _ in range(websocket_hook._CONSECUTIVE_PANE_UNSAFE_PINGS - 1):
                    await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
            assert websocket_hook._consecutive_ping_unsafe == (
                websocket_hook._CONSECUTIVE_PANE_UNSAFE_PINGS - 1
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
