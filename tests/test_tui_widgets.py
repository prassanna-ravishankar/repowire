"""Tests for TUI widgets."""

from __future__ import annotations

from repowire.tui.services.daemon_client import PeerInfo
from repowire.tui.widgets.agent_list import AgentList


def make_peer(
    name: str,
    status: str = "online",
    circle: str = "test",
    tmux_session: str | None = "0:test",
) -> PeerInfo:
    """Helper to create PeerInfo with default values."""
    return PeerInfo(
        peer_id=f"pane:{name}",
        name=name,
        display_name=name,
        status=status,
        circle=circle,
        backend="claude-code",
        path="/tmp",
        tmux_session=tmux_session,
        metadata={},
    )


class TestAgentList:
    """Tests for AgentList widget."""

    def test_id_to_peer_mapping(self) -> None:
        """Test that _id_to_peer maps option IDs to peers after rebuild."""
        agent_list = AgentList()
        peers = [
            make_peer("peer1", circle="alpha"),
            make_peer("peer2", circle="beta"),
        ]
        agent_list.agents = peers
        agent_list._rebuild()

        assert "agent_pane:peer1" in agent_list._id_to_peer
        assert "agent_pane:peer2" in agent_list._id_to_peer
        assert agent_list._id_to_peer["agent_pane:peer1"].name == "peer1"

    def test_peers_grouped_by_circle(self) -> None:
        """Test that peers from different circles are all in the mapping."""
        agent_list = AgentList()
        peers = [
            make_peer("peer1", circle="alpha"),
            make_peer("peer2", circle="beta"),
            make_peer("peer3", circle="alpha"),
        ]
        agent_list.agents = peers
        agent_list._rebuild()

        assert len(agent_list._id_to_peer) == 3

    def test_selected_agent_none_when_empty(self) -> None:
        """Test selected_agent returns None with no agents."""
        agent_list = AgentList()
        assert agent_list.selected_agent is None
