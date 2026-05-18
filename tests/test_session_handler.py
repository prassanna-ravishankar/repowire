"""Tests for the session hook handler."""

import json
import signal
from pathlib import Path
from unittest.mock import patch

from repowire.hooks.session_handler import (
    format_peers_context,
    get_peer_name,
    main,
)


def _run_with_input(input_data: dict) -> int:
    with patch("sys.stdin") as mock_stdin:
        mock_stdin.read.return_value = json.dumps(input_data)
        return main()


class TestGetPeerName:
    def test_folder_name(self):
        assert get_peer_name("/Users/prass/projects/repowire") == "repowire"

    def test_nested_path(self):
        assert get_peer_name("/a/b/c/myproject") == "myproject"


class TestFormatPeersContext:
    def test_empty_peers(self):
        assert format_peers_context([], "me") == ""

    def test_only_self(self):
        peers = [{"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}}]
        assert format_peers_context(peers, "me") == ""

    def test_formats_online_peers(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {
                "name": "other", "status": "online",
                "path": "/tmp/other", "metadata": {"branch": "main"},
            },
        ]
        result = format_peers_context(peers, "me")
        assert "other" in result
        assert "main" in result
        assert "@dashboard" in result
        assert "set_description" in result

    def test_excludes_offline(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {"name": "offline-peer", "status": "offline", "path": "/tmp/off", "metadata": {}},
        ]
        result = format_peers_context(peers, "me")
        assert result == ""

    def test_shows_description(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {
                "name": "worker",
                "status": "online",
                "path": "/tmp/worker",
                "metadata": {},
                "description": "fixing auth",
            },
        ]
        result = format_peers_context(peers, "me")
        assert "fixing auth" in result


class TestSessionMain:
    def test_invalid_json(self):
        with patch("sys.stdin") as mock_stdin:
            mock_stdin.read.return_value = "not json"
            assert main() == 0

    def test_session_end_is_noop(self):
        result = _run_with_input({
            "hook_event_name": "SessionEnd",
            "cwd": "/tmp/test",
        })
        assert result == 0

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    def test_session_start_registers(
        self, mock_branch, mock_status, mock_popen, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            mock_register.assert_called_once()
            call_args = mock_register.call_args
            # First positional arg is now path (cwd), not display_name
            assert call_args[0][0] == str(tmp_path)

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_second_session_start_skips_ws_hook(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Repeated SessionStart for the same logical session skips ws-hook takeover."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": str(tmp_path),
                "hook_session_id": "eph99999-rest",
                "peer_id": "repow-default-abc12345",
            }))

            # Simulate a held flock — fcntl.flock raises OSError when lock is held
            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": str(tmp_path),
                    "session_id": "eph99999-rest",
                })

                # Should return 0 immediately — ws-hook alive, same project
                assert result == 0
                mock_register.assert_not_called()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "newproj-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_cwd_mismatch_kills_old_ws_hook(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Different cwd in same pane kills old ws-hook and re-registers."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.get_git_branch", return_value=None), \
             patch("repowire.hooks.session_handler.compute_git_status", return_value=None):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": "/old/project",
                "hook_session_id": "old-session",
                "peer_id": "repow-default-old12345",
            }))
            (log_dir / "ws-hook-1.pid").write_text("99999")

            new_cwd = str(tmp_path / "newproj")
            Path(new_cwd).mkdir()

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill, \
                 patch("repowire.hooks.session_handler.subprocess.Popen") as mock_popen:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                # First call (LOCK_NB) fails, second call (blocking) succeeds
                mock_fcntl.flock.side_effect = [
                    OSError("Resource temporarily unavailable"),
                    None,
                ]
                mock_popen.return_value.pid = 12345

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": new_cwd,
                    "session_id": "new-session",
                })

                assert result == 0
                mock_kill.assert_called_once_with(99999, signal.SIGTERM)
                mock_register.assert_called_once()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_same_project_new_session_takes_over(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Same cwd with a different hook session_id is treated as a fresh takeover."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.get_git_branch", return_value=None), \
             patch("repowire.hooks.session_handler.compute_git_status", return_value=None):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": str(tmp_path),
                "hook_session_id": "old-session",
                "peer_id": "repow-default-old12345",
            }))
            (log_dir / "ws-hook-1.pid").write_text("99999")
            (log_dir / "pending-query-1.json").write_text(json.dumps(["stale-cid"]))

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill, \
                 patch("repowire.hooks.session_handler.subprocess.Popen") as mock_popen:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = [
                    OSError("Resource temporarily unavailable"),
                    None,
                ]
                mock_popen.return_value.pid = 12345

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": str(tmp_path),
                    "session_id": "new-session",
                })

                assert result == 0
                mock_kill.assert_called_once_with(99999, signal.SIGTERM)
                mock_register.assert_called_once()
                assert not (log_dir / "pending-query-1.json").exists()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=4242)
    def test_session_start_sends_agent_pid_as_ppid_not_own_pid(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_popen,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """The agent_pid in the registration payload must be os.getppid(),
        not os.getpid() — the hook process dies seconds after registration,
        so storing its own pid makes the pane-hijack guard useless after
        the hook exits (issue #190 review)."""
        import os as os_mod

        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.os.getppid", return_value=31415):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            mock_register.assert_called_once()
            kwargs = mock_register.call_args.kwargs
            # agent_pid must be the AGENT's pid (the hook's parent), not the
            # hook's own pid. We patched getppid to 31415 — if the value is
            # os.getpid() (this pytest process), the test catches the bug.
            assert kwargs["agent_pid"] == 31415
            assert kwargs["agent_pid"] != os_mod.getpid()
            # parent_pid is computed by walking up one more step from the
            # agent. We mocked _read_ppid_of to 4242.
            assert kwargs["parent_pid"] == 4242
            mock_read_ppid.assert_called_once_with(31415)

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=(None, None, True),  # hijack_rejected=True
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=99999)
    def test_rejected_hijack_leaves_incumbent_untouched(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """When the daemon rejects a hijack (409), the hook must NOT kill the
        incumbent's ws-hook, NOT mark the prior peer offline, and NOT clear
        the pane runtime metadata. Issue #190 round-2 review."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler._mark_peer_offline") as mock_offline, \
             patch("repowire.hooks.session_handler.spawn_ws_hook") as mock_spawn, \
             patch("repowire.hooks.session_handler.write_pane_runtime_metadata") as mock_write:
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            incumbent_meta = {
                "backend": "claude-code",
                "cwd": "/incumbent/proj",
                "hook_session_id": "incumbent-session",
                "peer_id": "repow-default-incumbent",
                "display_name": "incumbent-claude-code",
            }
            meta_path = log_dir / "ws-hook-1.meta.json"
            pid_path = log_dir / "ws-hook-1.pid"
            meta_path.write_text(json.dumps(incumbent_meta))
            pid_path.write_text("12345")

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                # Incumbent holds the lock → first flock attempt fails.
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": "/hijacker/proj",
                    "session_id": "hijacker-session",
                })

            assert result == 0
            mock_register.assert_called_once()
            mock_kill.assert_not_called()
            mock_offline.assert_not_called()
            mock_spawn.assert_not_called()
            mock_write.assert_not_called()
            # Incumbent's on-disk state untouched.
            assert meta_path.exists()
            assert json.loads(meta_path.read_text()) == incumbent_meta
            assert pid_path.exists()
            assert pid_path.read_text() == "12345"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=(None, None, False),  # transport failure / 5xx: no accept, no 409
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=99999)
    def test_takeover_aborted_when_registration_unconfirmed(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """If the daemon doesn't 409 but also doesn't confirm acceptance
        (transport error, 5xx, malformed body), a pane takeover MUST NOT
        proceed — destroying the incumbent on every daemon hiccup defeats
        the hijack guard. See issue #190 round-3 review."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler._mark_peer_offline") as mock_offline, \
             patch("repowire.hooks.session_handler.spawn_ws_hook") as mock_spawn, \
             patch("repowire.hooks.session_handler.write_pane_runtime_metadata") as mock_write:
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            incumbent_meta = {
                "backend": "claude-code",
                "cwd": "/incumbent/proj",
                "hook_session_id": "incumbent-session",
                "peer_id": "repow-default-incumbent",
                "display_name": "incumbent-claude-code",
            }
            meta_path = log_dir / "ws-hook-1.meta.json"
            pid_path = log_dir / "ws-hook-1.pid"
            meta_path.write_text(json.dumps(incumbent_meta))
            pid_path.write_text("12345")

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": "/newcomer/proj",
                    "session_id": "newcomer-session",
                })

            assert result == 0
            mock_register.assert_called_once()
            mock_kill.assert_not_called()
            mock_offline.assert_not_called()
            mock_spawn.assert_not_called()
            mock_write.assert_not_called()
            assert meta_path.exists()
            assert json.loads(meta_path.read_text()) == incumbent_meta
            assert pid_path.exists()
            assert pid_path.read_text() == "12345"
