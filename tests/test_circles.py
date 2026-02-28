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


# ---------------------------------------------------------------------------
# Same-name peers in different circles
# ---------------------------------------------------------------------------


class TestSameNameDifferentCircles:
    """Tests that query/notify target the correct peer when two peers share a display_name."""

    @staticmethod
    async def _register(pm: PeerManager, session_id: str, name: str, circle: str) -> None:
        peer = Peer(
            peer_id=session_id,
            display_name=name,
            path=f"/{name}",
            machine="localhost",
            circle=circle,
        )
        await pm.register_peer(peer)

    async def test_query_targets_correct_circle(self, mock_message_router, mock_session_mapper):
        """query(..., circle='teamA') routes to the teamA peer, not teamB."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-a", "myproject", "teamA")
        await self._register(pm, "sid-b", "myproject", "teamB")

        await pm.query("cli", "myproject", "hello", circle="teamA")

        mock_message_router.send_query.assert_called_once()
        _, kwargs = mock_message_router.send_query.call_args
        assert kwargs["to_session_id"] == "sid-a"

    async def test_query_wrong_circle_raises(self, mock_message_router, mock_session_mapper):
        """query with circle that doesn't exist raises ValueError."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-a", "myproject", "teamA")

        with pytest.raises(ValueError, match="Unknown peer"):
            await pm.query("cli", "myproject", "hello", circle="teamZ")

    async def test_notify_targets_correct_circle(self, mock_message_router, mock_session_mapper):
        """notify(..., circle='teamB') routes to the teamB peer."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-a", "myproject", "teamA")
        await self._register(pm, "sid-b", "myproject", "teamB")

        await pm.notify("cli", "myproject", "hi", circle="teamB")

        mock_message_router.send_notification.assert_called_once()
        _, kwargs = mock_message_router.send_notification.call_args
        assert kwargs["to_session_id"] == "sid-b"

    async def test_notify_no_circle_picks_online_peer(
        self, mock_message_router, mock_session_mapper
    ):
        """notify with no circle falls back to online-first tiebreaking."""
        from repowire.protocol.peers import PeerStatus

        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-a", "myproject", "teamA")
        await self._register(pm, "sid-b", "myproject", "teamB")

        # Mark sid-a offline so sid-b wins the tiebreak
        async with pm._lock:
            pm._peers["sid-a"].status = PeerStatus.OFFLINE

        await pm.notify("cli", "myproject", "hi")

        mock_message_router.send_notification.assert_called_once()
        _, kwargs = mock_message_router.send_notification.call_args
        assert kwargs["to_session_id"] == "sid-b"

    async def test_circle_access_checked_with_resolved_peers(
        self, mock_message_router, mock_session_mapper
    ):
        """Circle check uses resolved Peer objects, not ambiguous display_names."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        # sender is in teamA; two "myproject" targets in teamA and teamB
        await self._register(pm, "sid-sender", "sender", "teamA")
        await self._register(pm, "sid-a", "myproject", "teamA")
        await self._register(pm, "sid-b", "myproject", "teamB")

        # Query teamA target — sender and target are in same circle: should succeed
        await pm.query("sender", "myproject", "hello", circle="teamA")
        mock_message_router.send_query.assert_called_once()

    async def test_cross_circle_blocked_with_resolved_peers(
        self, mock_message_router, mock_session_mapper
    ):
        """When target circle differs from sender circle, access is blocked."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-sender", "sender", "teamA")
        await self._register(pm, "sid-b", "myproject", "teamB")

        with pytest.raises(ValueError, match="Circle boundary"):
            await pm.query("sender", "myproject", "hello", circle="teamB")


# ---------------------------------------------------------------------------
# from_peer circle-preferred lookup (Fix 3 regression)
# ---------------------------------------------------------------------------


class TestFromPeerCircleLookup:
    """Regression tests: from_peer is resolved preferring target's circle first."""

    @staticmethod
    async def _register(pm: PeerManager, session_id: str, name: str, circle: str) -> None:
        peer = Peer(
            peer_id=session_id,
            display_name=name,
            path=f"/{name}",
            machine="localhost",
            circle=circle,
        )
        await pm.register_peer(peer)

    async def test_same_name_sender_in_same_circle_no_false_boundary(
        self, mock_message_router, mock_session_mapper
    ):
        """sender and target share display_name pattern; sender in same circle — no error."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        # Two senders with same display_name in different circles
        await self._register(pm, "sid-sender-a", "orchestrator", "teamA")
        await self._register(pm, "sid-sender-b", "orchestrator", "teamB")
        await self._register(pm, "sid-target", "worker", "teamA")

        # from_peer="orchestrator" should resolve to teamA (target's circle), not teamB
        result = await pm.query("orchestrator", "worker", "hello")
        assert result == "mock response"

    async def test_sender_circle_mismatch_still_blocked(
        self, mock_message_router, mock_session_mapper
    ):
        """If the only matching sender is in a different circle, boundary is enforced."""
        pm = PeerManager(Config(), mock_message_router, mock_session_mapper)
        await self._register(pm, "sid-sender", "orchestrator", "teamB")
        await self._register(pm, "sid-target", "worker", "teamA")

        with pytest.raises(ValueError, match="Circle boundary"):
            await pm.query("orchestrator", "worker", "hello")
