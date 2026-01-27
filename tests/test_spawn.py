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
        config = SpawnConfig(path="/home/user/myproject", circle="dev", backend="claudemux")
        assert config.display_name == "myproject"

    def test_display_name_nested_path(self) -> None:
        """Test display_name from nested path."""
        config = SpawnConfig(path="/home/user/git/frontend", circle="dev", backend="claudemux")
        assert config.display_name == "frontend"

    def test_display_name_trailing_slash(self) -> None:
        """Test display_name handles trailing slash."""
        config = SpawnConfig(path="/home/user/myproject/", circle="dev", backend="claudemux")
        # Path.name strips trailing slash
        assert config.display_name == "myproject"

    def test_default_command_empty(self) -> None:
        """Test default command is empty string."""
        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claudemux")
        assert config.command == ""

    def test_custom_command(self) -> None:
        """Test custom command is stored."""
        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend="claudemux",
            command="claude --model opus",
        )
        assert config.command == "claude --model opus"


class TestSpawnResult:
    """Tests for SpawnResult dataclass."""

    def test_spawn_result_fields(self) -> None:
        """Test SpawnResult has expected fields."""
        result = SpawnResult(
            pane_id="%42",
            display_name="myapp",
            tmux_session="default:myapp",
        )
        assert result.pane_id == "%42"
        assert result.display_name == "myapp"
        assert result.tmux_session == "default:myapp"
        assert result.registered is False  # Default

    def test_spawn_result_registered(self) -> None:
        """Test SpawnResult with registered=True."""
        result = SpawnResult(
            pane_id="%42",
            display_name="myapp",
            tmux_session="default:myapp",
            registered=True,
        )
        assert result.registered is True


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

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_creates_tmux_window(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
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
        mock_register.return_value = True

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claudemux")
        result = spawn_peer(config)

        assert result.pane_id == "%42"
        assert result.display_name == "test"
        assert result.tmux_session == "dev:test"
        assert result.registered is True
        mock_pane.send_keys.assert_called_once_with("claude", enter=True)

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_uses_custom_command(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
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
        mock_register.return_value = True

        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend="claudemux",
            command="claude --model opus",
        )
        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with("claude --model opus", enter=True)

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_opencode_backend(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
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
        mock_register.return_value = True

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="opencode")
        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with("opencode", enter=True)

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_handles_daemon_failure(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
    ) -> None:
        """Test spawn_peer returns registered=False when daemon fails."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session
        mock_register.return_value = False  # Daemon registration fails

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claudemux")
        result = spawn_peer(config)

        assert result.registered is False

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_unknown_backend_raises(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
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

        with pytest.raises(ValueError, match="Unknown backend"):
            spawn_peer(config)

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_no_active_pane_raises(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
    ) -> None:
        """Test spawn_peer raises when no active pane."""
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_window.active_pane = None
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = mock_session

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claudemux")

        with pytest.raises(RuntimeError, match="Failed to get active pane"):
            spawn_peer(config)

    @patch("repowire.spawn._register_with_daemon")
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_unique_window_name(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        mock_register: MagicMock,
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
        mock_register.return_value = True

        config = SpawnConfig(path="/tmp/test", circle="dev", backend="claudemux")
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


class TestRegisterWithDaemon:
    """Tests for _register_with_daemon helper."""

    def test_register_success(self, httpx_mock) -> None:
        """Test successful daemon registration."""
        from repowire.spawn import _register_with_daemon

        httpx_mock.add_response(
            url="http://127.0.0.1:8377/peers",
            method="POST",
            json={"status": "ok"},
        )

        with patch("repowire.spawn.get_config") as mock_config:
            mock_cfg = MagicMock()
            mock_cfg.daemon.host = "127.0.0.1"
            mock_cfg.daemon.port = 8377
            mock_config.return_value = mock_cfg

            result = _register_with_daemon(
                pane_id="%42",
                display_name="test",
                path="/tmp/test",
                tmux_session="dev:test",
                circle="dev",
                backend="claudemux",
            )

        assert result is True

    def test_register_http_error(self, httpx_mock) -> None:
        """Test daemon registration with HTTP error."""
        from repowire.spawn import _register_with_daemon

        httpx_mock.add_response(
            url="http://127.0.0.1:8377/peers",
            method="POST",
            status_code=500,
        )

        with patch("repowire.spawn.get_config") as mock_config:
            mock_cfg = MagicMock()
            mock_cfg.daemon.host = "127.0.0.1"
            mock_cfg.daemon.port = 8377
            mock_config.return_value = mock_cfg

            result = _register_with_daemon(
                pane_id="%42",
                display_name="test",
                path="/tmp/test",
                tmux_session="dev:test",
                circle="dev",
                backend="claudemux",
            )

        assert result is False

    def test_register_connection_error(self) -> None:
        """Test daemon registration with connection error."""
        import httpx

        from repowire.spawn import _register_with_daemon

        with patch("repowire.spawn.get_config") as mock_config:
            mock_cfg = MagicMock()
            mock_cfg.daemon.host = "127.0.0.1"
            mock_cfg.daemon.port = 9999  # Non-existent port
            mock_config.return_value = mock_cfg

            with patch("repowire.spawn.httpx.Client") as mock_client:
                mock_ctx = MagicMock()
                mock_ctx.post.side_effect = httpx.RequestError("Connection refused")
                mock_client.return_value.__enter__.return_value = mock_ctx
                mock_client.return_value.__exit__.return_value = None

                result = _register_with_daemon(
                    pane_id="%42",
                    display_name="test",
                    path="/tmp/test",
                    tmux_session="dev:test",
                    circle="dev",
                    backend="claudemux",
                )

        assert result is False


class TestListTmuxSessions:
    """Tests for list_tmux_sessions function."""

    @patch("repowire.spawn.libtmux.Server")
    def test_list_sessions_success(self, mock_server_class: MagicMock) -> None:
        """Test listing tmux sessions."""
        from repowire.spawn import list_tmux_sessions

        mock_server = mock_server_class.return_value
        mock_sessions = [MagicMock(), MagicMock()]
        mock_sessions[0].name = "dev"
        mock_sessions[1].name = "prod"
        mock_server.sessions = mock_sessions

        result = list_tmux_sessions()

        assert result == ["dev", "prod"]

    @patch("repowire.spawn.libtmux.Server")
    def test_list_sessions_empty(self, mock_server_class: MagicMock) -> None:
        """Test listing empty sessions."""
        from repowire.spawn import list_tmux_sessions

        mock_server = mock_server_class.return_value
        mock_server.sessions = []

        result = list_tmux_sessions()

        assert result == []

    @patch("repowire.spawn.libtmux.Server")
    def test_list_sessions_exception(self, mock_server_class: MagicMock) -> None:
        """Test returns empty list on exception."""
        from libtmux.exc import LibTmuxException

        from repowire.spawn import list_tmux_sessions

        mock_server_class.side_effect = LibTmuxException("no server")

        result = list_tmux_sessions()

        assert result == []

    @patch("repowire.spawn.libtmux.Server")
    def test_list_sessions_filters_none_names(self, mock_server_class: MagicMock) -> None:
        """Test filters out sessions with None names."""
        from repowire.spawn import list_tmux_sessions

        mock_server = mock_server_class.return_value
        mock_sessions = [MagicMock(), MagicMock(), MagicMock()]
        mock_sessions[0].name = "dev"
        mock_sessions[1].name = None
        mock_sessions[2].name = "prod"
        mock_server.sessions = mock_sessions

        result = list_tmux_sessions()

        assert result == ["dev", "prod"]
