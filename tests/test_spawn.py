"""Tests for spawn module."""

from __future__ import annotations

from unittest.mock import MagicMock, patch

import pytest

from repowire.spawn import (
    SpawnConfig,
    SpawnResult,
    _get_or_create_session,
    _unique_window_name,
    attach_session,
    kill_peer,
    spawn_peer,
)


class TestSpawnConfig:
    """Tests for SpawnConfig dataclass."""

    def test_display_name_from_path(self) -> None:
        """Test display_name derives from path."""
        config = SpawnConfig(path="/home/user/myproject", circle="dev", backend="claude-code")
        assert config.display_name == "myproject"

    def test_display_name_nested_path(self) -> None:
        """Test display_name from nested path."""
        config = SpawnConfig(path="/home/user/git/frontend", circle="dev", backend="claude-code")
        assert config.display_name == "frontend"

    def test_display_name_trailing_slash(self) -> None:
        """Test display_name handles trailing slash."""
        config = SpawnConfig(path="/home/user/myproject/", circle="dev", backend="claude-code")
        # Path.name strips trailing slash
        assert config.display_name == "myproject"

    def test_default_command_empty(self) -> None:
        """Test default command is empty string."""
        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claude-code")
        assert config.command == ""

    def test_custom_command(self) -> None:
        """Test custom command is stored."""
        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend="claude-code",
            command="claude --model opus",
        )
        assert config.command == "claude --model opus"


class TestSpawnResult:
    """Tests for SpawnResult dataclass."""

    def test_spawn_result_fields(self) -> None:
        """Test SpawnResult has expected fields."""
        result = SpawnResult(
            display_name="myapp",
            tmux_session="default:myapp",
        )
        assert result.display_name == "myapp"
        assert result.tmux_session == "default:myapp"


class TestUniqueWindowName:
    """Tests for _unique_window_name helper."""

    def test_unique_name_no_conflict(self) -> None:
        """Test returns base name when no conflict."""
        mock_session = MagicMock()
        mock_session.windows = []

        name = _unique_window_name(mock_session, "frontend")
        assert name == "frontend"

    def test_unique_name_with_conflict(self) -> None:
        """Test appends suffix when name exists."""
        mock_session = MagicMock()
        mock_window = MagicMock()
        mock_window.name = "frontend"
        mock_session.windows = [mock_window]

        name = _unique_window_name(mock_session, "frontend")
        assert name == "frontend-2"

    def test_unique_name_multiple_conflicts(self) -> None:
        """Test finds next available suffix."""
        mock_session = MagicMock()
        mock_windows = [MagicMock(), MagicMock(), MagicMock()]
        mock_windows[0].name = "frontend"
        mock_windows[1].name = "frontend-2"
        mock_windows[2].name = "frontend-3"
        mock_session.windows = mock_windows

        name = _unique_window_name(mock_session, "frontend")
        assert name == "frontend-4"

    def test_unique_name_gap_in_sequence(self) -> None:
        """Test finds first available suffix when there's a gap."""
        mock_session = MagicMock()
        mock_windows = [MagicMock(), MagicMock()]
        mock_windows[0].name = "frontend"
        mock_windows[1].name = "frontend-3"  # Gap at -2
        mock_session.windows = mock_windows

        name = _unique_window_name(mock_session, "frontend")
        assert name == "frontend-2"

    def test_unique_name_with_none_window_names(self) -> None:
        """Test handles windows with None names."""
        mock_session = MagicMock()
        mock_windows = [MagicMock(), MagicMock()]
        mock_windows[0].name = None  # Window without name
        mock_windows[1].name = "frontend"
        mock_session.windows = mock_windows

        name = _unique_window_name(mock_session, "frontend")
        assert name == "frontend-2"


class TestGetOrCreateSession:
    """Tests for _get_or_create_session helper."""

    @patch("repowire.spawn.libtmux.Server")
    def test_get_existing_session(self, mock_server_class: MagicMock) -> None:
        """Test returns existing session."""
        mock_server = MagicMock()
        mock_session = MagicMock()
        mock_server.sessions.get.return_value = mock_session

        result = _get_or_create_session(mock_server, "dev")

        assert result == mock_session
        mock_server.sessions.get.assert_called_once_with(session_name="dev")
        mock_server.new_session.assert_not_called()

    @patch("repowire.spawn.libtmux.Server")
    def test_create_new_session_when_not_exists(self, mock_server_class: MagicMock) -> None:
        """Test creates new session when not found."""
        mock_server = MagicMock()
        mock_server.sessions.get.return_value = None
        mock_new_session = MagicMock()
        mock_server.new_session.return_value = mock_new_session

        result = _get_or_create_session(mock_server, "dev")

        assert result == mock_new_session
        mock_server.new_session.assert_called_once_with(session_name="dev")

    @patch("repowire.spawn.libtmux.Server")
    def test_create_new_session_on_exception(self, mock_server_class: MagicMock) -> None:
        """Test creates new session when get raises exception."""
        from libtmux.exc import LibTmuxException

        mock_server = MagicMock()
        mock_server.sessions.get.side_effect = LibTmuxException("not found")
        mock_new_session = MagicMock()
        mock_server.new_session.return_value = mock_new_session

        result = _get_or_create_session(mock_server, "dev")

        assert result == mock_new_session
        mock_server.new_session.assert_called_once_with(session_name="dev")


class TestSpawnPeer:
    """Tests for spawn_peer function."""

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_creates_tmux_window(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer creates a tmux window."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claude-code")
        result = spawn_peer(config)

        assert result.display_name == "test"
        assert result.tmux_session == "dev:test"
        mock_pane.send_keys.assert_called_once_with("claude", enter=True)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_uses_custom_command(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer uses custom command when provided."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend="claude-code",
            command="claude --model opus",
        )
        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with("claude --model opus", enter=True)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_opencode_backend(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer uses opencode command for opencode backend."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="opencode")
        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with("opencode", enter=True)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_unknown_backend_raises(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer raises for unknown backend."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="unknown")

        with pytest.raises(ValueError, match="Unknown agent type"):
            spawn_peer(config)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_no_active_pane_raises(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer raises when no active pane."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_window.active_pane = None
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claude-code")

        with pytest.raises(RuntimeError, match="Failed to get active pane"):
            spawn_peer(config)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_unique_window_name(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer handles duplicate window names."""
        mock_session = MagicMock()
        mock_existing_window = MagicMock()
        mock_existing_window.name = "test"
        mock_session.windows = [mock_existing_window]
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claude-code")
        result = spawn_peer(config)

        assert result.display_name == "test-2"
        assert result.tmux_session == "dev:test-2"


class TestKillPeer:
    """Tests for kill_peer function."""

    def test_kill_peer_invalid_session_format(self) -> None:
        """Test returns False for invalid session format."""
        result = kill_peer("no-colon-here")
        assert result is False

    @patch("repowire.spawn.libtmux.Server")
    def test_kill_peer_session_not_found(self, mock_server_class: MagicMock) -> None:
        """Test returns False when session doesn't exist."""
        mock_server = mock_server_class.return_value
        mock_server.sessions.get.return_value = None

        result = kill_peer("dev:frontend")
        assert result is False

    @patch("repowire.spawn.libtmux.Server")
    def test_kill_peer_window_not_found(self, mock_server_class: MagicMock) -> None:
        """Test returns False when window doesn't exist."""
        mock_server = mock_server_class.return_value
        mock_session = MagicMock()
        mock_session.windows.get.return_value = None
        mock_server.sessions.get.return_value = mock_session

        result = kill_peer("dev:frontend")
        assert result is False

    @patch("repowire.spawn.libtmux.Server")
    def test_kill_peer_success(self, mock_server_class: MagicMock) -> None:
        """Test returns True when window is killed."""
        mock_server = mock_server_class.return_value
        mock_session = MagicMock()
        mock_window = MagicMock()
        mock_session.windows.get.return_value = mock_window
        mock_server.sessions.get.return_value = mock_session

        result = kill_peer("dev:frontend")

        assert result is True
        mock_window.kill.assert_called_once()

    @patch("repowire.spawn.libtmux.Server")
    def test_kill_peer_exception_returns_false(self, mock_server_class: MagicMock) -> None:
        """Test returns False when libtmux raises exception."""
        from libtmux.exc import LibTmuxException

        mock_server = mock_server_class.return_value
        mock_server.sessions.get.side_effect = LibTmuxException("error")

        result = kill_peer("dev:frontend")
        assert result is False


class TestAttachSession:
    """Tests for attach_session function."""

    @patch("repowire.spawn.subprocess.run")
    def test_attach_session_with_window(self, mock_run: MagicMock) -> None:
        """Test attach_session with session:window format."""
        attach_session("dev:frontend")

        assert mock_run.call_count == 2
        mock_run.assert_any_call(["tmux", "select-window", "-t", "dev:frontend"], check=False)
        mock_run.assert_any_call(["tmux", "attach-session", "-t", "dev"], check=True)

    @patch("repowire.spawn.subprocess.run")
    def test_attach_session_without_window(self, mock_run: MagicMock) -> None:
        """Test attach_session with session only."""
        attach_session("dev")

        assert mock_run.call_count == 2
        mock_run.assert_any_call(["tmux", "select-window", "-t", "dev"], check=False)
        mock_run.assert_any_call(["tmux", "attach-session", "-t", "dev"], check=True)


