"""Tests for spawn module."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from repowire.config.models import AgentType
from repowire.spawn import (
    SpawnConfig,
    SpawnResult,
    _get_or_create_session,
    _unique_window_name,
    attach_session,
    kill_pane,
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
            pane_id="%42",
        )
        assert result.display_name == "myapp"
        assert result.tmux_session == "default:myapp"
        assert result.pane_id == "%42"
        assert result.message is None


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

        result, created = _get_or_create_session(mock_server, "dev")

        assert result == mock_session
        assert created is False
        mock_server.sessions.get.assert_called_once_with(session_name="dev")
        mock_server.new_session.assert_not_called()

    @patch("repowire.spawn.libtmux.Server")
    def test_create_new_session_when_not_exists(self, mock_server_class: MagicMock) -> None:
        """Test creates new session when not found."""
        mock_server = MagicMock()
        mock_server.sessions.get.return_value = None
        mock_new_session = MagicMock()
        mock_server.new_session.return_value = mock_new_session

        result, created = _get_or_create_session(
            mock_server,
            "dev",
            start_directory="/tmp/project",
            window_name="project",
        )

        assert result == mock_new_session
        assert created is True
        mock_server.new_session.assert_called_once_with(
            session_name="dev",
            start_directory="/tmp/project",
            window_name="project",
        )

    @patch("repowire.spawn.libtmux.Server")
    def test_create_new_session_on_exception(self, mock_server_class: MagicMock) -> None:
        """Test creates new session when get raises exception."""
        from libtmux.exc import LibTmuxException

        mock_server = MagicMock()
        mock_server.sessions.get.side_effect = LibTmuxException("not found")
        mock_new_session = MagicMock()
        mock_server.new_session.return_value = mock_new_session

        result, created = _get_or_create_session(mock_server, "dev")

        assert result == mock_new_session
        assert created is True
        mock_server.new_session.assert_called_once_with(
            session_name="dev",
            start_directory=None,
            window_name=None,
        )


class TestSpawnPeer:
    """Tests for spawn_peer function."""

    @pytest.fixture(autouse=True)
    def _isolate_spawn_hints(self, tmp_path, monkeypatch: pytest.MonkeyPatch) -> None:
        """Redirect spawn-hint writes into a temp dir so tests don't leak files."""
        monkeypatch.setattr("repowire.spawn_hints.CACHE_DIR", tmp_path)

    def _tmux_spawn(self, mock_get_session: MagicMock, *, pane=True):
        mock_session = MagicMock()
        mock_session.windows = []
        mock_window = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window.active_pane = mock_pane if pane else None
        mock_session.new_window.return_value = mock_window
        mock_get_session.return_value = (mock_session, False)
        return mock_session, mock_window, mock_pane

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_creates_tmux_window(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer creates a tmux window."""
        _mock_session, _mock_window, mock_pane = self._tmux_spawn(mock_get_session)

        config = SpawnConfig(path="/tmp/test", circle="dev", backend=AgentType.CLAUDE_CODE)
        result = spawn_peer(config)

        assert result.display_name == "test"
        assert result.tmux_session == "dev:test"
        _mock_session.new_window.assert_called_once_with(
            window_name="test",
            start_directory="/tmp/test",
        )
        mock_pane.send_keys.assert_called_once_with(
            "claude --dangerously-skip-permissions", enter=True,
        )

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_uses_first_window_for_new_session(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """A brand-new tmux session must not leave a root-cwd default window."""
        mock_session = MagicMock()
        mock_pane = MagicMock()
        mock_pane.id = "%42"
        mock_window = MagicMock()
        mock_window.name = "test"
        mock_window.active_pane = mock_pane
        mock_session.windows = [mock_window]
        mock_get_session.return_value = (mock_session, True)

        config = SpawnConfig(path="/tmp/test", circle="dev", backend=AgentType.CLAUDE_CODE)
        result = spawn_peer(config)

        assert result.display_name == "test"
        assert result.tmux_session == "dev:test"
        mock_session.new_window.assert_not_called()
        mock_pane.send_keys.assert_called_once_with(
            "claude --dangerously-skip-permissions",
            enter=True,
        )

    @pytest.mark.parametrize(
        ("backend", "command", "expected"),
        [
            (AgentType.CLAUDE_CODE, "claude --model opus", "claude --model opus"),
            (AgentType.OPENCODE, "", "opencode"),
            (
                AgentType.CODEX,
                "",
                "codex --dangerously-bypass-approvals-and-sandbox",
            ),
        ],
    )
    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_sends_backend_command(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
        backend: AgentType,
        command: str,
        expected: str,
    ) -> None:
        """Test spawn_peer sends custom or backend-default command."""
        _mock_session, _mock_window, mock_pane = self._tmux_spawn(mock_get_session)
        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend=backend,
            command=command,
        )
        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with(expected, enter=True)

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_prefixes_explicit_env(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Spawned workers launch with explicit env instead of incidental tmux env."""
        _mock_session, _mock_window, mock_pane = self._tmux_spawn(mock_get_session)
        config = SpawnConfig(
            path="/tmp/test",
            circle="dev",
            backend=AgentType.CODEX,
            command="codex run",
            env={"PATH": "/Users/me/bin:/opt/homebrew/bin", "FOO": "two words"},
        )

        spawn_peer(config)

        mock_pane.send_keys.assert_called_once_with(
            "env FOO='two words' PATH=/Users/me/bin:/opt/homebrew/bin codex run",
            enter=True,
        )

    @patch("repowire.spawn._get_or_create_session")
    @patch("repowire.spawn.libtmux.Server")
    def test_spawn_peer_unknown_backend_raises(
        self,
        mock_server_class: MagicMock,
        mock_get_session: MagicMock,
    ) -> None:
        """Test spawn_peer raises for unknown backend."""
        self._tmux_spawn(mock_get_session)

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
        self._tmux_spawn(mock_get_session, pane=False)

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
        mock_get_session.return_value = (mock_session, False)

        config = SpawnConfig(path="/tmp/test", circle="dev", backend=AgentType.CLAUDE_CODE)
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


class TestKillPane:
    """Tests for kill_pane (stable-pane-id kill)."""

    def test_kill_pane_empty_id_returns_false(self) -> None:
        assert kill_pane("") is False

    @patch("repowire.spawn.subprocess.run")
    def test_kill_pane_success(self, mock_run: MagicMock) -> None:
        mock_run.return_value = MagicMock(returncode=0)
        assert kill_pane("%42") is True
        mock_run.assert_called_once_with(
            ["tmux", "kill-pane", "-t", "%42"],
            capture_output=True,
            text=True,
            check=False,
        )

    @patch("repowire.spawn.subprocess.run")
    def test_kill_pane_failure_returns_false(self, mock_run: MagicMock) -> None:
        mock_run.return_value = MagicMock(returncode=1)
        assert kill_pane("%99") is False

    @patch("repowire.spawn.subprocess.run")
    def test_kill_pane_oserror_returns_false(self, mock_run: MagicMock) -> None:
        mock_run.side_effect = OSError("tmux missing")
        assert kill_pane("%1") is False


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


class TestMcpToolDescriptions:
    """Tests for MCP tool descriptions containing disambiguation markers."""

    def test_mcp_tools_have_mesh_prefix(self) -> None:
        """All repowire MCP tools should include [Repowire mesh] in their description."""
        from repowire.mcp.server import create_mcp_server
        mcp = create_mcp_server()
        mesh_tools = ["list_peers", "ask", "ack", "notify_peer", "broadcast",
                       "spawn_peer", "kill_peer", "whoami", "set_description",
                       "claim_orchestrator_role", "schedule_create", "schedule_self",
                       "schedule_cron",
                       "schedule_list", "schedule_delete", "job_create", "job_list",
                       "job_status", "job_show", "job_update", "job_result",
                       "job_cancel"]
        for name in mesh_tools:
            tool = mcp._tool_manager._tools.get(name)
            assert tool is not None, f"Tool {name} not found"
            desc = tool.description or ""
            assert "[Repowire mesh]" in desc, (
                f"Tool {name} missing [Repowire mesh] prefix in description"
            )

    def test_addressing_tools_warn_about_sendmessage(self) -> None:
        """Tools that send messages should warn against using SendMessage."""
        from repowire.mcp.server import create_mcp_server
        mcp = create_mcp_server()
        for name in ["ask", "notify_peer", "broadcast", "spawn_peer"]:
            tool = mcp._tool_manager._tools.get(name)
            desc = tool.description or ""
            assert "SendMessage" in desc, (
                f"Tool {name} should mention SendMessage to prevent confusion"
            )


class TestMcpSpawnPeerReturn:
    """Tests for spawn_peer MCP tool return value."""

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_spawn_peer_returns_display_name_and_tmux_session(
        self, mock_request: AsyncMock,
    ) -> None:
        """spawn_peer MCP tool should return both display_name and tmux_session."""
        mock_request.return_value = {
            "ok": True,
            "display_name": "alpha-svc",
            "tmux_session": "prod:alpha-svc",
        }

        from repowire.mcp.server import create_mcp_server
        mcp = create_mcp_server()
        tools = {name: fn for name, fn in mcp._tool_manager._tools.items()}
        spawn_tool = tools["spawn_peer"]
        result = await spawn_tool.fn(
            path="/tmp/alpha-svc", backend="claude-code", circle="prod",
        )

        # Must mention both display_name and tmux_session distinctly
        assert "alpha-svc" in result
        assert "prod:alpha-svc" in result
        # Must NOT be just the raw tmux_session string
        assert result != "prod:alpha-svc"

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_spawn_peer_surfaces_antigravity_cli_fallback_warning(
        self, mock_request: AsyncMock,
    ) -> None:
        mock_request.return_value = {
            "ok": True,
            "display_name": "agy-demo",
            "tmux_session": "default:agy-demo",
            "peer_id": "repow-default-agy12345",
            "registration_state": "cli_fallback",
            "warnings": ["antigravity plugin hooks are pending upstream"],
        }

        from repowire.mcp.server import create_mcp_server

        mcp = create_mcp_server()
        spawn_tool = mcp._tool_manager._tools["spawn_peer"]
        result = await spawn_tool.fn(
            path="/tmp/agy-demo",
            backend="antigravity",
            circle="default",
        )

        assert "agy-demo" in result
        assert "repow-default-agy12345" in result
        assert "registration_state=cli_fallback" in result
        assert "pending upstream" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server._get_my_identity", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_spawn_peer_defaults_to_callers_circle(
        self,
        mock_request: AsyncMock,
        mock_identity: AsyncMock,
        mock_register: AsyncMock,
    ) -> None:
        """Omitting circle should spawn into the caller's current tmux circle."""
        mock_identity.return_value = ("repowire-codex", "agentbox", "agent")
        mock_request.return_value = {
            "ok": True,
            "display_name": "alpha-svc",
            "tmux_session": "agentbox:alpha-svc",
        }

        from repowire.mcp.server import create_mcp_server

        mcp = create_mcp_server()
        spawn_tool = mcp._tool_manager._tools["spawn_peer"]
        result = await spawn_tool.fn(path="/tmp/alpha-svc", backend="codex")

        mock_register.assert_awaited_once_with(strict=True)
        mock_identity.assert_awaited_once()
        mock_request.assert_awaited_once_with(
            "POST",
            "/spawn",
            {"path": "/tmp/alpha-svc", "circle": "agentbox", "backend": "codex"},
        )
        assert "agentbox:alpha-svc" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_spawn_peer_preserves_explicit_default_circle(
        self, mock_request: AsyncMock, mock_register: AsyncMock,
    ) -> None:
        """Explicit circle='default' should still target the default tmux session."""
        mock_request.return_value = {
            "ok": True,
            "display_name": "alpha-svc",
            "tmux_session": "default:alpha-svc",
        }

        from repowire.mcp.server import create_mcp_server

        mcp = create_mcp_server()
        spawn_tool = mcp._tool_manager._tools["spawn_peer"]
        result = await spawn_tool.fn(
            path="/tmp/alpha-svc", backend="codex", circle="default",
        )

        mock_register.assert_not_awaited()
        mock_request.assert_awaited_once_with(
            "POST",
            "/spawn",
            {"path": "/tmp/alpha-svc", "circle": "default", "backend": "codex"},
        )
        assert "default:alpha-svc" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_spawn_peer_posts_profile(
        self, mock_request: AsyncMock, mock_register: AsyncMock,
    ) -> None:
        """spawn_peer MCP tool should forward model/profile selection."""
        mock_request.return_value = {
            "ok": True,
            "display_name": "alpha-svc",
            "tmux_session": "default:alpha-svc",
        }

        from repowire.mcp.server import create_mcp_server

        mcp = create_mcp_server()
        spawn_tool = mcp._tool_manager._tools["spawn_peer"]
        result = await spawn_tool.fn(
            path="/tmp/alpha-svc",
            backend="codex",
            profile="fast",
            circle="default",
        )

        mock_register.assert_not_awaited()
        mock_request.assert_awaited_once_with(
            "POST",
            "/spawn",
            {
                "path": "/tmp/alpha-svc",
                "circle": "default",
                "backend": "codex",
                "profile": "fast",
            },
        )
        assert "default:alpha-svc" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server._get_my_peer_name", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_kill_peer_uses_peer_identifier_not_tmux_session(
        self, mock_request: AsyncMock, mock_my_name: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        """kill_peer MCP tool should send mesh identity to the safe kill route."""
        from repowire.mcp.server import create_mcp_server

        mock_my_name.return_value = "orchestrator"
        mock_request.return_value = {"ok": True, "tmux_killed": True}
        mcp = create_mcp_server()
        tools = {name: fn for name, fn in mcp._tool_manager._tools.items()}
        kill_tool = tools["kill_peer"]
        result = await kill_tool.fn(peer_identifier="repow-5-abc12345", circle="5")

        mock_request.assert_awaited_once_with(
            "POST",
            "/kill-peer",
            {
                "peer_identifier": "repow-5-abc12345",
                "from_peer": "orchestrator",
                "circle": "5",
            },
        )
        assert "Killed peer repow-5-abc12345 in circle 5" in result
        assert "tmux pane killed" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server._get_my_peer_name", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_kill_peer_surfaces_skipped_tmux_kill(
        self, mock_request: AsyncMock, mock_my_name: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        """When the daemon returns tmux_killed=None (ownership not proven),
        the MCP tool must tell the caller — not silently report success."""
        from repowire.mcp.server import create_mcp_server

        mock_my_name.return_value = "orchestrator"
        mock_request.return_value = {"ok": True, "tmux_killed": None}
        mcp = create_mcp_server()
        kill_tool = {name: fn for name, fn in mcp._tool_manager._tools.items()}["kill_peer"]
        result = await kill_tool.fn(peer_identifier="repow-5-abc12345")

        assert "Killed peer repow-5-abc12345" in result
        assert "skipped" in result.lower()
        assert "ownership not proven" in result.lower()
        assert "tmux kill-pane" in result

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server._get_my_peer_name", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_kill_peer_surfaces_failed_tmux_kill(
        self, mock_request: AsyncMock, mock_my_name: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        """When the daemon returns tmux_killed=False (kill attempted but failed),
        the MCP tool must surface the orphan-pane risk."""
        from repowire.mcp.server import create_mcp_server

        mock_my_name.return_value = "orchestrator"
        mock_request.return_value = {"ok": True, "tmux_killed": False}
        mcp = create_mcp_server()
        kill_tool = {name: fn for name, fn in mcp._tool_manager._tools.items()}["kill_peer"]
        result = await kill_tool.fn(peer_identifier="repow-5-abc12345")

        assert "Killed peer repow-5-abc12345" in result
        assert "failed" in result.lower()
        assert "tmux list-panes" in result


class TestMcpRegistration:
    """Tests for MCP lazy registration behavior."""

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.read_runtime_birth_certificate", return_value=[])
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    @patch(
        "repowire.mcp.server.get_tmux_info",
        return_value={"pane_id": "%1", "session_name": "0", "window_name": "repowire"},
    )
    async def test_tmux_lazy_registration_uses_circle_without_claiming_pane(
        self, _mock_tmux, mock_request: AsyncMock, _mock_birth_certificate,
    ) -> None:
        """MCP fallback registration should keep tmux circle without taking pane ownership."""
        import repowire.mcp.server as mcp_server

        mcp_server._registered = False
        mcp_server._cached_peer_name = None
        mock_request.side_effect = [
            Exception("not found"),  # /peers/by-pane lookup
            {"peers": []},  # /peers?path&backend fallback
            {"display_name": "repowire-codex"},  # POST /peers
            {"ok": True},  # POST /peers/repowire-codex/touch
        ]

        # patch.dict merges with the ambient env. When the suite runs INSIDE a
        # Claude Code session, CLAUDECODE/CLAUDE_CODE_*/AI_AGENT leak in and
        # claude-code's mcp_runtime_matches wins over the intended codex PATH
        # signal. Null those markers so detection sees only the codex PATH.
        with patch.dict(
            "repowire.mcp.server.os.environ",
            {
                "PATH": "/tmp/.codex/bin",
                "CLAUDECODE": "",
                "CLAUDE_CODE_SESSION_ID": "",
                "CLAUDE_CODE_ENTRYPOINT": "",
                "AI_AGENT": "",
                "GEMINI_CLI": "",
            },
        ):
            await mcp_server._ensure_registered()

        assert mock_request.await_count == 4
        assert mock_request.await_args_list[0].args == ("GET", "/peers/by-pane/%251")
        cwd = mcp_server.Path.cwd()
        assert mock_request.await_args_list[2].args == (
            "POST",
            "/peers",
            {
                "name": cwd.name or "root",
                "path": str(cwd),
                "circle": "0",
                "backend": "codex",
                "circle_source": "tmux",
            },
        )
        assert mock_request.await_args_list[3].args == (
            "POST", "/peers/repowire-codex/touch",
        )
        assert mcp_server._cached_peer_name == "repowire-codex"

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    @patch("repowire.mcp.server.get_pane_id", return_value=None)
    @patch(
        "repowire.mcp.server.get_tmux_info",
        return_value={"pane_id": None, "session_name": None, "window_name": None},
    )
    async def test_lazy_registration_adopts_existing_peer_by_path_backend(
        self, _mock_tmux, _mock_pane, mock_request: AsyncMock,
    ) -> None:
        """When pane lookup fails, MCP should adopt hook-registered peer matching path+backend."""
        import repowire.mcp.server as mcp_server

        mcp_server._registered = False
        mcp_server._cached_peer_name = None
        mock_request.side_effect = [
            Exception("name lookup fails"),  # GET /peers/<cwd-name>
            {"peers": [{"display_name": "torale-seo", "tmux_session": "0:0"}]},
            {"ok": True},  # POST /peers/torale-seo/touch
        ]

        # patch.dict merges with the ambient env. When the suite runs INSIDE a
        # Claude Code session, CLAUDECODE/CLAUDE_CODE_*/AI_AGENT leak in and
        # claude-code's mcp_runtime_matches wins over the intended codex PATH
        # signal. Null those markers so detection sees only the codex PATH.
        with patch.dict(
            "repowire.mcp.server.os.environ",
            {
                "PATH": "/tmp/.codex/bin",
                "CLAUDECODE": "",
                "CLAUDE_CODE_SESSION_ID": "",
                "CLAUDE_CODE_ENTRYPOINT": "",
                "AI_AGENT": "",
                "GEMINI_CLI": "",
            },
        ):
            await mcp_server._ensure_registered()

        assert mcp_server._cached_peer_name == "torale-seo"
        assert mcp_server._registered is True
        # Should NOT have made a POST to register a duplicate peer (touch is fine).
        register_posts = [
            c for c in mock_request.await_args_list
            if c.args[0] == "POST" and c.args[1] == "/peers"
        ]
        assert len(register_posts) == 0
        # Path+backend query should have been made
        get_calls = [c for c in mock_request.await_args_list if c.args[0] == "GET"]
        path_backend_call = next(
            (c for c in get_calls if c.args[1] == "/peers" and "params" in c.kwargs),
            None,
        )
        assert path_backend_call is not None
        assert path_backend_call.kwargs["params"]["backend"] == "codex"

        mcp_server._registered = False
        mcp_server._cached_peer_name = None

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.read_pane_runtime_metadata")
    @patch("repowire.mcp.server.os.getppid", return_value=12345)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    @patch(
        "repowire.mcp.server.get_tmux_info",
        return_value={"pane_id": "%1", "session_name": "0", "window_name": "repowire"},
    )
    async def test_existing_pane_peer_skips_registration(
        self, _mock_tmux, mock_request: AsyncMock, _mock_getppid, mock_meta,
    ) -> None:
        """If the pane already has a peer, MCP should not create a duplicate."""
        import repowire.mcp.server as mcp_server

        mcp_server._registered = False
        mcp_server._cached_peer_name = None
        mock_meta.return_value = {
            "peer_id": "repow-0-abc12345",
            "display_name": "repowire-codex",
            "backend": mcp_server._detect_backend(),
            "agent_pid": 12345,
        }
        mock_request.side_effect = [
            {"peer_id": "repow-0-abc12345", "display_name": "repowire-codex"},
            {"ok": True},  # POST /peers/repow-0-abc12345/touch
        ]

        await mcp_server._ensure_registered()

        assert mock_request.await_count == 2
        assert mock_request.await_args_list[0].args == ("GET", "/peers/by-pane/%251")
        assert mock_request.await_args_list[1].args == (
            "POST", "/peers/repow-0-abc12345/touch",
        )
        assert mcp_server._cached_peer_name == "repowire-codex"

        mcp_server._registered = False
        mcp_server._cached_peer_name = None

    @pytest.mark.asyncio
    @patch("repowire.mcp.server.read_pane_runtime_metadata")
    @patch("repowire.mcp.server.os.getppid", return_value=12345)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    @patch(
        "repowire.mcp.server.get_tmux_info",
        return_value={"pane_id": "%1", "session_name": "0", "window_name": "repowire"},
    )
    async def test_strict_tmux_registration_raises_when_hook_peer_is_missing(
        self,
        _mock_tmux,
        mock_request: AsyncMock,
        _mock_getppid,
        mock_meta,
    ) -> None:
        """Hook-managed tmux peers should not silently re-register over HTTP."""
        import repowire.mcp.server as mcp_server

        mcp_server._registered = False
        mcp_server._cached_peer_name = None
        mock_request.side_effect = [Exception("not found")]
        mock_meta.return_value = {
            "peer_id": "repow-0-abc12345",
            "display_name": "repowire-codex",
            "backend": mcp_server._detect_backend(),
            "agent_pid": 12345,
        }

        with pytest.raises(RuntimeError, match="inbound transport is disconnected"):
            await mcp_server._ensure_registered(strict=True)

        assert mock_request.await_count == 1
        post_calls = [c for c in mock_request.await_args_list if c.args[0] == "POST"]
        assert post_calls == []
        assert mcp_server._cached_peer_name == "repowire-codex"


class TestMcpListPeersSelfFilter:
    """list_peers should hide the calling peer by default (QoL)."""

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_list_peers_hides_self_by_default(
        self, mock_request: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        import repowire.mcp.server as mcp_server
        mcp_server._cached_peer_name = "orchestrator"
        mock_request.return_value = {
            "peers": [
                {"peer_id": "repow-1-aa", "display_name": "orchestrator",
                 "circle": "main", "status": "online", "backend": "claude-code"},
                {"peer_id": "repow-1-bb", "display_name": "alpha",
                 "circle": "main", "status": "online", "backend": "claude-code"},
            ]
        }

        try:
            mcp = mcp_server.create_mcp_server()
            list_tool = mcp._tool_manager._tools["list_peers"]
            result = await list_tool.fn()
            assert "alpha" in result
            assert "orchestrator" not in result
        finally:
            mcp_server._cached_peer_name = None

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_list_peers_include_self_opt_in(
        self, mock_request: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        import repowire.mcp.server as mcp_server
        mcp_server._cached_peer_name = "orchestrator"
        mock_request.return_value = {
            "peers": [
                {"peer_id": "repow-1-aa", "display_name": "orchestrator",
                 "circle": "main", "status": "online", "backend": "claude-code"},
                {"peer_id": "repow-1-bb", "display_name": "alpha",
                 "circle": "main", "status": "online", "backend": "claude-code"},
            ]
        }

        try:
            mcp = mcp_server.create_mcp_server()
            list_tool = mcp._tool_manager._tools["list_peers"]
            result = await list_tool.fn(include_self=True)
            assert "orchestrator" in result
            assert "alpha" in result
        finally:
            mcp_server._cached_peer_name = None

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_list_peers_no_self_filter_when_name_unknown(
        self, mock_request: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        """If _cached_peer_name is None, no filtering happens (don't accidentally drop peers)."""
        import repowire.mcp.server as mcp_server
        mcp_server._cached_peer_name = None
        mock_request.return_value = {
            "peers": [
                {"peer_id": "repow-1-aa", "display_name": "alpha",
                 "circle": "main", "status": "online", "backend": "claude-code"},
                {"peer_id": "repow-1-bb", "display_name": "beta",
                 "circle": "main", "status": "online", "backend": "claude-code"},
            ]
        }

        mcp = mcp_server.create_mcp_server()
        list_tool = mcp._tool_manager._tools["list_peers"]
        result = await list_tool.fn()
        assert "alpha" in result
        assert "beta" in result


class TestMcpListPeersLastSeen:
    """list_peers TSV must include last_seen + turn_state as trailing columns."""

    @pytest.mark.asyncio
    @patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock)
    @patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock)
    async def test_list_peers_includes_last_seen_column(
        self, mock_request: AsyncMock, _mock_register: AsyncMock,
    ) -> None:
        import repowire.mcp.server as mcp_server
        mcp_server._cached_peer_name = None
        mock_request.return_value = {
            "peers": [
                {"peer_id": "repow-1-aa", "display_name": "alpha",
                 "circle": "main", "status": "online", "backend": "claude-code",
                 "last_seen": "2026-05-14T12:00:00+00:00",
                 "turn_state": "awaiting_input"},
                {"peer_id": "repow-1-bb", "display_name": "beta",
                 "circle": "main", "status": "online", "backend": "claude-code",
                 "last_seen": None,
                 "turn_state": None},
            ]
        }

        mcp = mcp_server.create_mcp_server()
        list_tool = mcp._tool_manager._tools["list_peers"]
        result = await list_tool.fn()
        lines = result.splitlines()
        header = lines[0].split("\t")
        # turn_state followed last_seen for the pre-model layout; observed model
        # is appended so older fields keep their positions.
        assert header[-3] == "last_seen"
        assert header[-2] == "turn_state"
        assert header[-1] == "model"
        alpha_row = next(line for line in lines[1:] if "alpha" in line).split("\t")
        beta_row = next(line for line in lines[1:] if "beta" in line).split("\t")
        assert alpha_row[-3] == "2026-05-14T12:00:00+00:00"
        assert alpha_row[-2] == "awaiting_input"
        assert alpha_row[-1] == ""
        assert beta_row[-3] == ""
        assert beta_row[-2] == ""
        assert beta_row[-1] == ""


class TestRunMcpServer:
    """Sanity check that run_mcp_server enters the stdio loop."""

    @pytest.mark.asyncio
    async def test_run_mcp_server_runs_stdio(self) -> None:
        import repowire.mcp.server as mcp_server

        stdio_called = False
        mock_mcp = MagicMock()

        async def fake_stdio() -> None:
            nonlocal stdio_called
            stdio_called = True

        mock_mcp.run_stdio_async = fake_stdio

        with patch.object(mcp_server, "create_mcp_server", return_value=mock_mcp):
            await mcp_server.run_mcp_server()

        assert stdio_called is True
