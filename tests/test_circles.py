"""Tests for circles (logical subnet) feature."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from repowire.backends.base import Backend
from repowire.backends.claudemux.backend import ClaudemuxBackend
from repowire.config.models import Config, PeerConfig
from repowire.daemon.core import PeerManager
from repowire.protocol.peers import Peer, PeerStatus


class TestDeriveCircle:
    """Tests for circle derivation."""

    def test_base_backend_derive_circle_returns_global(self):
        """Base backend should return 'global' as default circle."""

        class TestBackend(Backend):
            name = "test"

            async def start(self):
                pass

            async def stop(self):
                pass

            async def send_message(self, peer, text):
                pass

            async def send_query(self, peer, text, timeout=120.0):
                return ""

            def get_peer_status(self, peer):
                return PeerStatus.ONLINE

        backend = TestBackend()
        peer_config = PeerConfig(name="test-peer")

        assert backend.derive_circle(peer_config) == "global"

    def test_claudemux_derive_circle_from_tmux_session(self):
        """Claudemux backend should derive circle from tmux session name."""
        backend = ClaudemuxBackend()

        # Session with window: "myproject:frontend" -> circle = "myproject"
        peer_config = PeerConfig(name="frontend", tmux_session="myproject:frontend")
        assert backend.derive_circle(peer_config) == "myproject"

        # Session without window: "myproject" -> circle = "myproject"
        peer_config2 = PeerConfig(name="backend", tmux_session="myproject")
        assert backend.derive_circle(peer_config2) == "myproject"

    def test_claudemux_derive_circle_without_tmux_returns_global(self):
        """Claudemux backend should return 'global' if no tmux session."""
        backend = ClaudemuxBackend()
        peer_config = PeerConfig(name="test-peer")

        assert backend.derive_circle(peer_config) == "global"


class TestCircleResolution:
    """Tests for circle resolution in PeerManager."""

    @pytest.fixture
    def mock_backend(self):
        """Create a mock backend."""
        backend = MagicMock(spec=Backend)
        backend.name = "test"
        backend.derive_circle = MagicMock(return_value="derived-circle")
        backend.get_peer_status = MagicMock(return_value=PeerStatus.ONLINE)
        return backend

    @pytest.fixture
    def mock_config(self):
        """Create a mock config."""
        config = MagicMock(spec=Config)
        config.peers = {}
        config.get_peer = MagicMock(return_value=None)
        return config

    def test_explicit_circle_overrides_derived(self, mock_backend, mock_config):
        """Explicit circle in config should override derived circle."""
        peer_manager = PeerManager(mock_backend, mock_config)

        # Peer with explicit circle
        peer_config = PeerConfig(name="test-peer", circle="explicit-circle")

        circle = peer_manager.resolve_circle(peer_config)

        assert circle == "explicit-circle"
        # Backend's derive_circle should not be called
        mock_backend.derive_circle.assert_not_called()

    def test_derived_circle_when_no_explicit(self, mock_backend, mock_config):
        """Should use backend derivation when no explicit circle."""
        peer_manager = PeerManager(mock_backend, mock_config)

        # Peer without explicit circle
        peer_config = PeerConfig(name="test-peer")

        circle = peer_manager.resolve_circle(peer_config)

        assert circle == "derived-circle"
        mock_backend.derive_circle.assert_called_once_with(peer_config)


class TestCircleAccessControl:
    """Tests for circle-based access control."""

    @pytest.fixture
    def mock_backend(self):
        """Create a mock backend."""
        backend = MagicMock(spec=Backend)
        backend.name = "test"
        backend.get_peer_status = MagicMock(return_value=PeerStatus.ONLINE)
        # Default derive_circle returns circle from config or "global"
        backend.derive_circle = MagicMock(
            side_effect=lambda p: p.circle or "global"
        )
        return backend

    def test_same_circle_allowed(self, mock_backend):
        """Peers in the same circle should be able to communicate."""
        config = Config()
        config.peers = {
            "peer-a": PeerConfig(name="peer-a", circle="my-circle"),
            "peer-b": PeerConfig(name="peer-b", circle="my-circle"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)
            # Should not raise
            peer_manager._check_circle_access("peer-a", "peer-b")

    def test_different_circle_blocked(self, mock_backend):
        """Peers in different circles should not be able to communicate."""
        config = Config()
        config.peers = {
            "peer-a": PeerConfig(name="peer-a", circle="circle-a"),
            "peer-b": PeerConfig(name="peer-b", circle="circle-b"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)

            with pytest.raises(ValueError, match="Circle boundary"):
                peer_manager._check_circle_access("peer-a", "peer-b")

    def test_cli_bypasses_circle(self, mock_backend):
        """CLI (from_peer='cli') should bypass circle restrictions."""
        config = Config()
        config.peers = {
            "cli": PeerConfig(name="cli", circle="circle-a"),
            "peer-b": PeerConfig(name="peer-b", circle="circle-b"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)
            # Should not raise - CLI always bypasses
            peer_manager._check_circle_access("cli", "peer-b")

    def test_bypass_flag_overrides_circle(self, mock_backend):
        """bypass=True should skip circle check."""
        config = Config()
        config.peers = {
            "peer-a": PeerConfig(name="peer-a", circle="circle-a"),
            "peer-b": PeerConfig(name="peer-b", circle="circle-b"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)
            # Should not raise with bypass=True
            peer_manager._check_circle_access("peer-a", "peer-b", bypass=True)


class TestBroadcastCircleFiltering:
    """Tests for broadcast filtering by circle."""

    @pytest.fixture
    def mock_backend(self):
        """Create a mock backend."""
        backend = MagicMock(spec=Backend)
        backend.name = "test"
        backend.get_peer_status = MagicMock(return_value=PeerStatus.ONLINE)
        backend.send_message = AsyncMock()
        backend.start = AsyncMock()
        backend.stop = AsyncMock()
        # Default derive_circle returns circle from config or "global"
        backend.derive_circle = MagicMock(
            side_effect=lambda p: p.circle or "global"
        )
        return backend

    async def test_broadcast_filters_by_circle(self, mock_backend):
        """Broadcast should only reach peers in the same circle."""
        config = Config()
        config.peers = {
            "sender": PeerConfig(name="sender", circle="my-circle", path="/sender"),
            "peer-a": PeerConfig(name="peer-a", circle="my-circle", path="/a"),
            "peer-b": PeerConfig(name="peer-b", circle="other-circle", path="/b"),
            "peer-c": PeerConfig(name="peer-c", circle="my-circle", path="/c"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)
            await peer_manager.start()

            sent_to = await peer_manager.broadcast("sender", "hello")

            # Should only send to peer-a and peer-c (same circle)
            assert sorted(sent_to) == ["peer-a", "peer-c"]
            assert "peer-b" not in sent_to

    async def test_broadcast_bypass_sends_to_all(self, mock_backend):
        """Broadcast with bypass_circle=True should reach all circles."""
        config = Config()
        config.peers = {
            "sender": PeerConfig(name="sender", circle="my-circle", path="/sender"),
            "peer-a": PeerConfig(name="peer-a", circle="my-circle", path="/a"),
            "peer-b": PeerConfig(name="peer-b", circle="other-circle", path="/b"),
        }

        with patch("repowire.daemon.core.load_config", return_value=config):
            peer_manager = PeerManager(mock_backend, config)
            await peer_manager.start()

            sent_to = await peer_manager.broadcast("sender", "hello", bypass_circle=True)

            # Should send to both peers
            assert sorted(sent_to) == ["peer-a", "peer-b"]


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

    def test_config_add_peer_with_circle(self):
        """Config.add_peer should accept circle parameter."""
        with patch.object(Config, "save"):
            config = Config()
            config.add_peer(name="test", path="/test", circle="my-circle")
            assert config.peers["test"].circle == "my-circle"
