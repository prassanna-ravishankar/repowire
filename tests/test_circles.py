"""Tests for circles (logical subnet) feature.

Covers: data models (Peer, PeerConfig), and access control via the public query() API.
Circle enforcement now uses the live peer registry (not config.yaml).
"""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import Config, PeerConfig
from repowire.daemon.core import PeerManager
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.session_mapper import SessionMapper
from repowire.protocol.peers import Peer

# ---------------------------------------------------------------------------
# Shared fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
def mock_message_router():
    """Mock MessageRouter – send_query returns a canned response."""
    router = MagicMock(spec=MessageRouter)
    router.send_query = AsyncMock(return_value="mock response")
    router.send_notification = AsyncMock()
    router.broadcast = AsyncMock(return_value=[])
    return router


@pytest.fixture
def mock_session_mapper():
    """Mock SessionMapper – no persisted sessions."""
    mapper = MagicMock(spec=SessionMapper)
    mapper.get_all_mappings.return_value = {}
    return mapper


@pytest.fixture
def make_peer_manager(mock_message_router, mock_session_mapper):
    """Factory fixture: create a PeerManager with the given Config."""

    def _make(config: Config | None = None) -> PeerManager:
        return PeerManager(
            config or Config(),
            mock_message_router,
            mock_session_mapper,
        )

    return _make


# ---------------------------------------------------------------------------
# Peer model – circle field
# ---------------------------------------------------------------------------


class TestPeerCircleField:
    """Tests for circle field in Peer model."""

    def test_peer_default_circle_is_global(self):
        """Peer model should have 'global' as default circle."""
        peer = Peer(name="test", path="/test", machine="localhost")
        assert peer.circle == "global"

    def test_peer_circle_in_to_dict(self):
        """Peer.to_dict() should include circle."""
        peer = Peer(name="test", path="/test", machine="localhost", circle="my-circle")
        data = peer.to_dict()
        assert data["circle"] == "my-circle"

    def test_peer_circle_from_dict(self):
        """Peer.from_dict() should preserve circle."""
        data = {
            "name": "test",
            "path": "/test",
            "machine": "localhost",
            "circle": "my-circle",
            "status": "online",
        }
        peer = Peer.from_dict(data)
        assert peer.circle == "my-circle"


# ---------------------------------------------------------------------------
# PeerConfig – circle field
# ---------------------------------------------------------------------------


class TestPeerConfigCircle:
    """Tests for circle field in PeerConfig."""

    def test_peer_config_circle_field(self):
        """PeerConfig should have optional circle field."""
        peer_config = PeerConfig(name="test", circle="my-circle")
        assert peer_config.circle == "my-circle"

    def test_peer_config_circle_default_none(self):
        """PeerConfig circle should default to None."""
        peer_config = PeerConfig(name="test")
        assert peer_config.circle is None


# ---------------------------------------------------------------------------
# Circle access control (tested through public query() API)
# Now enforced from live peer registry, not config.yaml
# ---------------------------------------------------------------------------


class TestCircleAccessControl:
    """Tests for circle-based access control via query()."""

    @staticmethod
    async def _register(pm: PeerManager, name: str, circle: str) -> None:
        """Register a peer with the given name and circle."""
        peer = Peer(
            peer_id=f"repow-{circle}-{name}",
            display_name=name,
            path=f"/{name}",
            machine="localhost",
            circle=circle,
        )
        await pm.register_peer(peer)

    async def test_same_circle_query_succeeds(self, mock_message_router, mock_session_mapper):
        """Peers in the same circle can query each other."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "peer-a", "dev")
        await self._register(pm, "peer-b", "dev")

        result = await pm.query("peer-a", "peer-b", "hello")
        assert result == "mock response"

    async def test_cross_circle_query_blocked(self, mock_message_router, mock_session_mapper):
        """Peers in different circles cannot query each other."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "peer-a", "dev")
        await self._register(pm, "peer-b", "staging")

        with pytest.raises(ValueError, match="Circle boundary"):
            await pm.query("peer-a", "peer-b", "hello")

    async def test_bypass_circle_query_succeeds(self, mock_message_router, mock_session_mapper):
        """bypass_circle=True allows cross-circle queries."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "peer-a", "dev")
        await self._register(pm, "peer-b", "staging")

        result = await pm.query("peer-a", "peer-b", "hello", bypass_circle=True)
        assert result == "mock response"

    async def test_unknown_peer_no_enforcement(self, mock_message_router, mock_session_mapper):
        """Unknown sender peer = no circle enforcement (CLI callers)."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "peer-b", "staging")

        # "cli" is not registered, so no enforcement
        result = await pm.query("cli", "peer-b", "hello")
        assert result == "mock response"
