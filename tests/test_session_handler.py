"""Tests for the session hook handler."""

import json
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
    @patch("repowire.hooks.session_handler._register_peer_http", return_value=True)
    @patch("repowire.hooks.session_handler.get_tmux_info",
           return_value={"pane_id": "%1", "session_name": "default", "window_name": "test"})
    def test_session_start_registers(self, mock_tmux, mock_register, mock_fetch, tmp_path):
        with patch("repowire.hooks.session_handler.CACHE_DIR", tmp_path):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            mock_register.assert_called_once()
            call_args = mock_register.call_args
            assert call_args[0][0] == "abc12345"  # display_name from session_id[:8]

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch("repowire.hooks.session_handler._register_peer_http", return_value=True)
    @patch("repowire.hooks.session_handler.get_tmux_info",
           return_value={"pane_id": "%1", "session_name": "default", "window_name": "test"})
    def test_second_session_start_skips_ws_hook(self, mock_tmux, mock_register, mock_fetch, tmp_path):
        """Second SessionStart for same pane skips ws-hook spawn (flock dedup)."""
        with patch("repowire.hooks.session_handler.CACHE_DIR", tmp_path):
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

                # Should return 0 immediately — ws-hook alive, skip entirely
                assert result == 0
                mock_register.assert_not_called()
