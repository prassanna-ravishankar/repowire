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
        return_value=("repow-default-abc12345", "test-claude-code"),
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
    def test_session_start_registers(
        self, mock_popen, mock_tmux, mock_register, mock_fetch, tmp_path,
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
        return_value=("repow-default-abc12345", "test-claude-code"),
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
        return_value=("repow-default-abc12345", "newproj-claude-code"),
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
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
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
        return_value=("repow-default-abc12345", "test-claude-code"),
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
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": str(tmp_path),
                "hook_session_id": "old-session",
                "peer_id": "repow-default-old12345",
            }))
            (log_dir / "ws-hook-1.pid").write_text("99999")
            (log_dir / "pending-1.json").write_text(json.dumps(["stale-cid"]))

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
                assert not (log_dir / "pending-1.json").exists()
