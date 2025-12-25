import asyncio
import subprocess
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, Mock, patch

import pytest
from mcp.server.fastmcp.exceptions import ToolError

from repowire.mesh.server import create_mcp_server, get_transport


async def call_tool_raw(mcp, name, arguments):
    """Helper to call tools and get raw results by calling the function directly."""
    # Get the tool's function from the registered tools
    tool = mcp._tool_manager.get_tool(name)
    if tool is None:
        raise ValueError(f"Tool {name} not found")
    # Call the function directly
    return await tool.fn(**arguments)


class TestListSessions:
    """Test the list_sessions MCP tool."""

    @pytest.mark.asyncio
    async def test_list_sessions_basic(self):
        mock_transport = AsyncMock()
        mock_transport.list_sessions.return_value = [
            {
                "id": "session-1",
                "path": "/path/to/project1",
                "host": "localhost",
                "active": True,
                "metadata": {"path": "/path/to/project1", "host": "localhost"},
            },
            {
                "id": "session-2",
                "path": "/path/to/project2",
                "host": "localhost",
                "active": False,
                "metadata": {"path": "/path/to/project2", "host": "localhost"},
            },
        ]

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,"list_sessions", {})

        assert len(result) == 2
        assert result[0]["id"] == "session-1"
        assert result[0]["path"] == "/path/to/project1"
        assert result[0]["active"] is True
        assert result[1]["id"] == "session-2"
        assert result[1]["active"] is False
        mock_transport.list_sessions.assert_called_once()

    @pytest.mark.asyncio
    async def test_list_sessions_empty(self):
        mock_transport = AsyncMock()
        mock_transport.list_sessions.return_value = []

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,"list_sessions", {})

        assert result == []
        mock_transport.list_sessions.assert_called_once()

    @pytest.mark.asyncio
    async def test_list_sessions_with_metadata(self):
        mock_transport = AsyncMock()
        mock_transport.list_sessions.return_value = [
            {
                "id": "session-xyz",
                "path": "/home/user/project",
                "host": "desktop-pc",
                "active": True,
                "metadata": {
                    "path": "/home/user/project",
                    "host": "desktop-pc",
                    "version": "1.0.0",
                },
            }
        ]

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,"list_sessions", {})

        assert len(result) == 1
        assert result[0]["metadata"]["version"] == "1.0.0"


class TestSendMessage:
    """Test the send_message MCP tool."""

    @pytest.mark.asyncio
    async def test_send_message_default_permission(self):
        mock_transport = AsyncMock()
        mock_transport.send_message.return_value = "Response from session"

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,
                "send_message",
                {"session_id": "session-123", "text": "Hello, world!"},
            )

        assert result == "Response from session"
        mock_transport.send_message.assert_called_once_with(
            "session-123", "Hello, world!", "default"
        )

    @pytest.mark.asyncio
    async def test_send_message_plan_mode(self):
        mock_transport = AsyncMock()
        mock_transport.send_message.return_value = "Planned response"

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,
                "send_message",
                {
                    "session_id": "session-456",
                    "text": "What should we do?",
                    "permission_mode": "plan",
                },
            )

        assert result == "Planned response"
        mock_transport.send_message.assert_called_once_with(
            "session-456", "What should we do?", "plan"
        )

    @pytest.mark.asyncio
    async def test_send_message_yolo_mode(self):
        mock_transport = AsyncMock()
        mock_transport.send_message.return_value = "Done!"

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            result = await call_tool_raw(mcp,
                "send_message",
                {
                    "session_id": "session-789",
                    "text": "Deploy to production",
                    "permission_mode": "yolo",
                },
            )

        assert result == "Done!"
        mock_transport.send_message.assert_called_once_with(
            "session-789", "Deploy to production", "yolo"
        )

    @pytest.mark.asyncio
    async def test_send_message_all_permission_modes(self):
        """Test all documented permission modes."""
        permission_modes = [
            "default",
            "plan",
            "yolo",
            "bypassPermissions",
            "acceptEdits",
            "read-only",
            "safe-yolo",
        ]

        for mode in permission_modes:
            mock_transport = AsyncMock()
            mock_transport.send_message.return_value = f"Response in {mode}"

            with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
                mcp = create_mcp_server()
                result = await call_tool_raw(mcp,
                    "send_message",
                    {
                        "session_id": "session-test",
                        "text": "Test message",
                        "permission_mode": mode,
                    },
                )

            assert result == f"Response in {mode}"
            mock_transport.send_message.assert_called_once_with(
                "session-test", "Test message", mode
            )

    @pytest.mark.asyncio
    async def test_send_message_timeout(self):
        mock_transport = AsyncMock()
        mock_transport.send_message.side_effect = asyncio.TimeoutError("Request timed out")

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            with pytest.raises(asyncio.TimeoutError, match="Request timed out"):
                await call_tool_raw(mcp, "send_message",
                    {"session_id": "session-slow", "text": "Slow query"})

    @pytest.mark.asyncio
    async def test_send_message_session_not_found(self):
        mock_transport = AsyncMock()
        mock_transport.send_message.side_effect = ValueError("Session not found")

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            with pytest.raises(ValueError, match="Session not found"):
                await call_tool_raw(mcp, "send_message",
                    {"session_id": "nonexistent", "text": "Hello"})


class TestCreateSession:
    """Test the create_session MCP tool."""

    @pytest.mark.asyncio
    async def test_create_session_success(self):
        mock_transport = AsyncMock()

        # Mock list_sessions to return empty first, then the new session
        before_sessions = []
        after_sessions = [
            {
                "id": "new-session-123",
                "path": "/test/path",
                "host": "localhost",
                "active": True,
                "metadata": {"path": "/test/path"},
            }
        ]

        mock_transport.list_sessions.side_effect = [before_sessions, after_sessions]

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            with patch("subprocess.Popen") as mock_popen:
                mock_process = Mock()
                mock_popen.return_value = mock_process

                mcp = create_mcp_server()
                result = await call_tool_raw(mcp,"create_session", {"path": "/test/path"})

        assert result["id"] == "new-session-123"
        assert result["path"] == "/test/path"
        assert result["active"] is True
        mock_popen.assert_called_once()
        call_args = mock_popen.call_args
        assert call_args[0][0] == ["happy"]
        assert call_args[1]["cwd"] == "/test/path"
        assert call_args[1]["stdout"] == subprocess.DEVNULL
        assert call_args[1]["stderr"] == subprocess.DEVNULL

    @pytest.mark.asyncio
    async def test_create_session_multiple_polls(self):
        """Test that create_session polls multiple times before finding session."""
        mock_transport = AsyncMock()

        # Return empty 3 times, then return the session
        empty_list = []
        session_list = [
            {
                "id": "delayed-session",
                "path": "/delayed/path",
                "host": "localhost",
                "active": True,
                "metadata": {"path": "/delayed/path"},
            }
        ]

        mock_transport.list_sessions.side_effect = [
            empty_list,
            empty_list,
            empty_list,
            session_list,
        ]

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            with patch("subprocess.Popen") as mock_popen:
                with patch("asyncio.sleep", new_callable=AsyncMock) as mock_sleep:
                    mcp = create_mcp_server()
                    result = await call_tool_raw(mcp,
                        "create_session", {"path": "/delayed/path"}
                    )

        assert result["id"] == "delayed-session"
        assert mock_transport.list_sessions.call_count == 4
        assert mock_sleep.call_count == 3

    @pytest.mark.asyncio
    async def test_create_session_timeout(self):
        """Test that create_session raises TimeoutError after 30 attempts."""
        mock_transport = AsyncMock()
        mock_transport.list_sessions.return_value = []

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            with patch("subprocess.Popen") as mock_popen:
                with patch("asyncio.sleep", new_callable=AsyncMock):
                    mcp = create_mcp_server()
                    with pytest.raises(
                        TimeoutError,
                        match="Session at /timeout/path did not appear within 30 seconds",
                    ):
                        await call_tool_raw(mcp, "create_session", {"path": "/timeout/path"})

        assert mock_transport.list_sessions.call_count == 31  # Before + 30 polls

    @pytest.mark.asyncio
    async def test_create_session_filters_by_path(self):
        """Test that create_session only returns sessions matching the path."""
        mock_transport = AsyncMock()

        before_sessions = [
            {"id": "existing-1", "path": "/other/path", "metadata": {"path": "/other/path"}}
        ]
        after_sessions = [
            {"id": "existing-1", "path": "/other/path", "metadata": {"path": "/other/path"}},
            {"id": "new-one", "path": "/target/path", "metadata": {"path": "/target/path"}},
            {"id": "another-new", "path": "/wrong/path", "metadata": {"path": "/wrong/path"}},
        ]

        mock_transport.list_sessions.side_effect = [before_sessions, after_sessions]

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            with patch("subprocess.Popen"):
                mcp = create_mcp_server()
                result = await call_tool_raw(mcp,"create_session", {"path": "/target/path"})

        assert result["id"] == "new-one"
        assert result["path"] == "/target/path"


class TestTransportSingleton:
    """Test the get_transport singleton pattern."""

    def test_get_transport_returns_same_instance(self):
        """Test that get_transport returns the same instance."""
        # Reset the global transport
        import repowire.mesh.server as server_module
        server_module._transport = None

        with patch("repowire.mesh.server.HappyTransport") as mock_happy:
            mock_instance = Mock()
            mock_happy.return_value = mock_instance

            transport1 = get_transport()
            transport2 = get_transport()

        assert transport1 is transport2
        mock_happy.assert_called_once()

    def test_get_transport_creates_instance_on_first_call(self):
        """Test that get_transport creates HappyTransport on first call."""
        import repowire.mesh.server as server_module
        server_module._transport = None

        with patch("repowire.mesh.server.HappyTransport") as mock_happy:
            mock_instance = Mock()
            mock_happy.return_value = mock_instance

            result = get_transport()

        assert result is mock_instance
        mock_happy.assert_called_once()


class TestMCPServerIntegration:
    """Integration tests for the MCP server."""

    @pytest.mark.asyncio
    async def test_multiple_tool_calls(self):
        """Test calling multiple tools in sequence."""
        mock_transport = AsyncMock()
        mock_transport.list_sessions.return_value = [
            {"id": "session-1", "path": "/path1", "active": True, "metadata": {}}
        ]
        mock_transport.send_message.return_value = "Response text"

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()

            # List sessions
            sessions = await call_tool_raw(mcp, "list_sessions", {})
            assert len(sessions) == 1

            # Send message
            response = await call_tool_raw(mcp, "send_message",
                {"session_id": "session-1", "text": "Hello"})
            assert response == "Response text"

        assert mock_transport.list_sessions.call_count == 1
        assert mock_transport.send_message.call_count == 1

    @pytest.mark.asyncio
    async def test_server_creation(self):
        """Test that create_mcp_server returns a valid FastMCP instance."""
        mcp = create_mcp_server()
        assert mcp is not None
        assert hasattr(mcp, "call_tool")

    @pytest.mark.asyncio
    async def test_error_propagation(self):
        """Test that transport errors propagate correctly."""
        mock_transport = AsyncMock()
        mock_transport.send_message.side_effect = ConnectionError("Network error")

        with patch("repowire.mesh.server.get_transport", return_value=mock_transport):
            mcp = create_mcp_server()
            with pytest.raises(ConnectionError, match="Network error"):
                await call_tool_raw(mcp, "send_message",
                    {"session_id": "session-1", "text": "Test"})
