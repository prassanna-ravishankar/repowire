"""Tests for TUI services."""

from __future__ import annotations

import httpx
import pytest

from repowire.spawn import SpawnConfig, SpawnResult
from repowire.tui.services.daemon_client import DaemonClient, PeerInfo


class TestDaemonClient:
    """Tests for DaemonClient."""

    @pytest.mark.asyncio
    async def test_health_success(self, httpx_mock) -> None:
        """Test successful health check."""
        httpx_mock.add_response(
            url="http://127.0.0.1:8377/health",
            json={
                "status": "ok",
                "version": "0.1.0",
                "backend": "claudemux",
                "relay_mode": False,
            },
        )

        async with DaemonClient() as client:
            health = await client.health()

        assert health is not None
        assert health.status == "ok"
        assert health.backend == "claudemux"

    @pytest.mark.asyncio
    async def test_get_peers_empty(self, httpx_mock) -> None:
        """Test get_peers with empty response."""
        httpx_mock.add_response(
            url="http://127.0.0.1:8377/peers",
            json={"peers": []},
        )

        async with DaemonClient() as client:
            peers = await client.get_peers()

        assert peers == []

    @pytest.mark.asyncio
    async def test_get_peers_with_data(self, httpx_mock) -> None:
        """Test get_peers with peer data."""
        httpx_mock.add_response(
            url="http://127.0.0.1:8377/peers",
            json={
                "peers": [
                    {
                        "name": "frontend",
                        "status": "online",
                        "circle": "myteam",
                        "path": "/tmp/frontend",
                        "tmux_session": "myteam:frontend",
                    }
                ]
            },
        )

        async with DaemonClient() as client:
            peers = await client.get_peers()

        assert len(peers) == 1
        assert peers[0].name == "frontend"
        assert peers[0].status == "online"

    @pytest.mark.asyncio
    async def test_health_connection_error(self, httpx_mock) -> None:
        """Test health check with connection error."""
        httpx_mock.add_exception(httpx.ConnectError("Connection refused"))

        async with DaemonClient() as client:
            health = await client.health()

        assert health is None


class TestPeerInfo:
    """Tests for PeerInfo dataclass."""

    def test_backend_tmux(self) -> None:
        """Test PeerInfo with claudemux backend."""
        peer = PeerInfo(
            pane_id="%42",
            name="test",
            display_name="test",
            status="online",
            circle="global",
            backend="claudemux",
            path="/tmp",
            tmux_session="0:test",
            opencode_url=None,
            metadata={},
        )
        assert peer.backend == "claudemux"
        assert peer.tmux_session == "0:test"

    def test_backend_opencode(self) -> None:
        """Test PeerInfo with opencode backend."""
        peer = PeerInfo(
            pane_id="opencode:123",
            name="test",
            display_name="test",
            status="online",
            circle="global",
            backend="opencode",
            path="/tmp",
            tmux_session=None,
            opencode_url="http://localhost:4096",
            metadata={},
        )
        assert peer.backend == "opencode"
        assert peer.opencode_url == "http://localhost:4096"

    def test_peerinfo_all_fields(self) -> None:
        """Test PeerInfo with all fields set."""
        peer = PeerInfo(
            pane_id="%99",
            name="myapp",
            display_name="My Application",
            status="busy",
            circle="development",
            backend="claudemux",
            path="/home/user/myapp",
            tmux_session="dev:myapp",
            opencode_url=None,
            metadata={"branch": "main"},
        )
        assert peer.pane_id == "%99"
        assert peer.name == "myapp"
        assert peer.display_name == "My Application"
        assert peer.status == "busy"
        assert peer.circle == "development"
        assert peer.metadata == {"branch": "main"}


class TestSpawnConfig:
    """Tests for SpawnConfig dataclass."""

    def test_default_command(self) -> None:
        """Test default command is empty string."""
        config = SpawnConfig(
            path="/tmp/testproject",
            circle="default",
            backend="claudemux",
        )
        assert config.command == ""

    def test_custom_command(self) -> None:
        """Test custom command."""
        config = SpawnConfig(
            path="/tmp/testproject",
            circle="default",
            backend="claudemux",
            command="claude --model opus",
        )
        assert config.command == "claude --model opus"

    def test_display_name_derived_from_path(self) -> None:
        """Test display_name is derived from path."""
        config = SpawnConfig(
            path="/home/user/projects/myapp",
            circle="default",
            backend="claudemux",
        )
        assert config.display_name == "myapp"

    def test_command_default_empty(self) -> None:
        """Test command defaults to empty string."""
        config = SpawnConfig(
            path="/tmp/test",
            circle="default",
            backend="claudemux",
        )
        assert config.command == ""

    def test_command_with_flags(self) -> None:
        """Test command with custom flags."""
        config = SpawnConfig(
            path="/tmp/test",
            circle="default",
            backend="claudemux",
            command="claude --model opus --verbose",
        )
        assert config.command == "claude --model opus --verbose"


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
