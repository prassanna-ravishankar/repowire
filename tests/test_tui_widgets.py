"""Tests for TUI widgets."""

from __future__ import annotations

from repowire.tui.services.daemon_client import PeerInfo
from repowire.tui.widgets.peer_list import PeerList


def make_peer(
    name: str,
    status: str = "online",
    circle: str = "test",
    tmux_session: str | None = "0:test",
) -> PeerInfo:
    """Helper to create PeerInfo with default values."""
    return PeerInfo(
        pane_id=f"pane:{name}",
        name=name,
        display_name=name,
        status=status,
        circle=circle,
        backend="claudemux",
        path="/tmp",
        tmux_session=tmux_session,
        opencode_url=None,
        metadata={},
    )


class TestPeerList:
    """Tests for PeerList widget."""

    def test_visible_peers_sorted_by_circle_then_status(self) -> None:
        """Test that visible peers are sorted by circle, then status within circle."""
        peer_list = PeerList()
        peer_list.show_offline = True  # Show all peers
        peers = [
            make_peer("offline1", status="offline", circle="beta", tmux_session=None),
            make_peer("online1", status="online", circle="beta", tmux_session="0:test"),
            make_peer("alpha_busy", status="busy", circle="alpha", tmux_session="0:test2"),
        ]
        peer_list._all_peers = peers

        # Should be sorted: alpha circle first, then beta (online, busy, offline)
        visible = peer_list._visible_peers
        assert visible[0].name == "alpha_busy"  # alpha circle
        assert visible[1].name == "online1"  # beta circle, online
        assert visible[2].name == "offline1"  # beta circle, offline

    def test_offline_hidden_by_default(self) -> None:
        """Test that offline peers are hidden by default."""
        peer_list = PeerList()
        peers = [
            make_peer("offline1", status="offline"),
            make_peer("online1", status="online"),
        ]
        peer_list._all_peers = peers

        # Offline hidden by default
        assert len(peer_list._visible_peers) == 1
        assert peer_list._visible_peers[0].name == "online1"

        # Show offline
        peer_list.show_offline = True
        assert len(peer_list._visible_peers) == 2

    def test_get_selected_peer_none_when_not_highlighted(self) -> None:
        """Test getting selected peer when nothing is highlighted."""
        peer_list = PeerList()
        peers = [make_peer("peer1")]
        peer_list._all_peers = peers

        # No highlight returns None
        assert peer_list.get_selected_peer() is None
        assert peer_list.get_filter_peer_name() is None

    def test_peers_property_stores_all_peers(self) -> None:
        """Test that peers property correctly stores all peers."""
        peer_list = PeerList()
        peers = [
            make_peer("peer1"),
            make_peer("peer2", status="busy"),
        ]
        peer_list.peers = peers

        assert len(peer_list.peers) == 2
        assert peer_list.peers[0].name == "peer1"
        assert peer_list.peers[1].name == "peer2"

    def test_option_to_peer_mapping(self) -> None:
        """Test that _option_to_peer correctly maps option IDs to peers."""
        peer_list = PeerList()
        peers = [
            make_peer("peer1", circle="alpha"),
            make_peer("peer2", circle="beta"),
        ]
        peer_list.peers = peers

        # After setting peers, the mapping should contain the peers
        assert "peer_peer1" in peer_list._option_to_peer
        assert "peer_peer2" in peer_list._option_to_peer
        assert peer_list._option_to_peer["peer_peer1"].name == "peer1"
        assert peer_list._option_to_peer["peer_peer2"].name == "peer2"

    def test_toggle_offline_changes_state(self) -> None:
        """Test that toggle_offline action changes the show_offline state."""
        peer_list = PeerList()
        assert peer_list.show_offline is False

        peer_list.action_toggle_offline()
        assert peer_list.show_offline is True

        peer_list.action_toggle_offline()
        assert peer_list.show_offline is False

    def test_peers_grouped_by_circle_in_mapping(self) -> None:
        """Test that peers from different circles are all in the mapping."""
        peer_list = PeerList()
        peers = [
            make_peer("peer1", circle="alpha"),
            make_peer("peer2", circle="beta"),
            make_peer("peer3", circle="alpha"),
        ]
        peer_list.peers = peers

        # All peers should be in the mapping
        assert len(peer_list._option_to_peer) == 3
        assert "peer_peer1" in peer_list._option_to_peer
        assert "peer_peer2" in peer_list._option_to_peer
        assert "peer_peer3" in peer_list._option_to_peer
