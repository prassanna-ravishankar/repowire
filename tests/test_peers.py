import pytest

from repowire.mesh.peers import Peer, PeerRegistry


class TestPeer:
    def test_peer_creation(self):
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-123",
            path="/path/to/backend",
            capabilities=["api", "database"],
        )
        assert peer.name == "backend"
        assert peer.agent_type == "opencode"
        assert peer.session_id == "session-123"
        assert peer.path == "/path/to/backend"
        assert peer.capabilities == ["api", "database"]
        assert peer.is_active is True

    def test_peer_default_capabilities(self):
        peer = Peer(
            name="test",
            agent_type="happy",
            session_id="session-456",
            path="/path/to/test",
        )
        assert peer.capabilities == []


class TestPeerRegistry:
    def test_register_peer(self):
        registry = PeerRegistry()
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-123",
            path="/path/to/backend",
        )
        peers = registry.register(peer)
        assert len(peers) == 1
        assert peers[0].name == "backend"

    def test_register_multiple_peers(self):
        registry = PeerRegistry()
        peer1 = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-1",
            path="/path/to/backend",
        )
        peer2 = Peer(
            name="frontend",
            agent_type="happy",
            session_id="session-2",
            path="/path/to/frontend",
        )
        registry.register(peer1)
        peers = registry.register(peer2)
        assert len(peers) == 2

    def test_unregister_peer(self):
        registry = PeerRegistry()
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-123",
            path="/path/to/backend",
        )
        registry.register(peer)
        assert registry.unregister("backend") is True
        assert registry.get("backend") is None

    def test_unregister_nonexistent_peer(self):
        registry = PeerRegistry()
        assert registry.unregister("nonexistent") is False

    def test_get_peer(self):
        registry = PeerRegistry()
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-123",
            path="/path/to/backend",
        )
        registry.register(peer)
        retrieved = registry.get("backend")
        assert retrieved is not None
        assert retrieved.name == "backend"

    def test_get_nonexistent_peer(self):
        registry = PeerRegistry()
        assert registry.get("nonexistent") is None

    def test_list_all_peers(self):
        registry = PeerRegistry()
        peer1 = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-1",
            path="/path/to/backend",
        )
        peer2 = Peer(
            name="frontend",
            agent_type="happy",
            session_id="session-2",
            path="/path/to/frontend",
        )
        registry.register(peer1)
        registry.register(peer2)
        peers = registry.list_all()
        assert len(peers) == 2
        names = [p.name for p in peers]
        assert "backend" in names
        assert "frontend" in names

    def test_get_by_session_id(self):
        registry = PeerRegistry()
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="unique-session-id",
            path="/path/to/backend",
        )
        registry.register(peer)
        retrieved = registry.get_by_session_id("unique-session-id")
        assert retrieved is not None
        assert retrieved.name == "backend"

    def test_get_by_session_id_nonexistent(self):
        registry = PeerRegistry()
        assert registry.get_by_session_id("nonexistent") is None

    def test_set_active(self):
        registry = PeerRegistry()
        peer = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-123",
            path="/path/to/backend",
        )
        registry.register(peer)
        assert registry.set_active("backend", False) is True
        retrieved = registry.get("backend")
        assert retrieved is not None
        assert retrieved.is_active is False

    def test_set_active_nonexistent(self):
        registry = PeerRegistry()
        assert registry.set_active("nonexistent", True) is False

    def test_names_property(self):
        registry = PeerRegistry()
        peer1 = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-1",
            path="/path/to/backend",
        )
        peer2 = Peer(
            name="frontend",
            agent_type="happy",
            session_id="session-2",
            path="/path/to/frontend",
        )
        registry.register(peer1)
        registry.register(peer2)
        names = registry.names
        assert len(names) == 2
        assert "backend" in names
        assert "frontend" in names

    def test_register_replaces_existing(self):
        registry = PeerRegistry()
        peer1 = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-1",
            path="/path/to/backend",
        )
        peer2 = Peer(
            name="backend",
            agent_type="opencode",
            session_id="session-2",
            path="/new/path/to/backend",
        )
        registry.register(peer1)
        registry.register(peer2)
        peers = registry.list_all()
        assert len(peers) == 1
        assert peers[0].session_id == "session-2"
        assert peers[0].path == "/new/path/to/backend"
