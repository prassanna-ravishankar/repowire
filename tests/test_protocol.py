import pytest
from datetime import datetime

from repowire.protocol.peers import Peer, PeerStatus
from repowire.protocol.messages import (
    Message,
    MessageType,
    QueryMessage,
    ResponseMessage,
    NotificationMessage,
    BroadcastMessage,
)


class TestPeer:
    def test_create_peer(self):
        peer = Peer(
            name="backend",
            path="/app/backend",
            machine="laptop",
            tmux_session="claude-backend",
        )

        assert peer.name == "backend"
        assert peer.path == "/app/backend"
        assert peer.machine == "laptop"
        assert peer.tmux_session == "claude-backend"
        assert peer.status == PeerStatus.OFFLINE
        assert peer.is_local() is True

    def test_peer_to_dict(self):
        peer = Peer(
            name="frontend",
            path="/app/frontend",
            machine="desktop",
            status=PeerStatus.ONLINE,
        )

        data = peer.to_dict()

        assert data["name"] == "frontend"
        assert data["status"] == "online"
        assert data["tmux_session"] is None

    def test_peer_from_dict(self):
        data = {
            "name": "api",
            "path": "/app/api",
            "machine": "server",
            "status": "busy",
        }

        peer = Peer.from_dict(data)

        assert peer.name == "api"
        assert peer.status == PeerStatus.BUSY


class TestMessages:
    def test_query_message(self):
        msg = QueryMessage.create(
            from_peer="frontend",
            to_peer="backend",
            text="What's the API schema?",
        )

        assert msg.type == MessageType.QUERY
        assert msg.from_peer == "frontend"
        assert msg.to_peer == "backend"
        assert msg.payload["text"] == "What's the API schema?"
        assert msg.correlation_id is not None

    def test_response_message(self):
        msg = ResponseMessage.create(
            from_peer="backend",
            to_peer="frontend",
            text='{"id": "string"}',
            correlation_id="abc123",
        )

        assert msg.type == MessageType.RESPONSE
        assert msg.correlation_id == "abc123"
        assert msg.payload["success"] is True

    def test_notification_message(self):
        msg = NotificationMessage.create(
            from_peer="ci",
            to_peer="backend",
            text="Build complete",
        )

        assert msg.type == MessageType.NOTIFICATION
        assert msg.correlation_id is None

    def test_broadcast_message(self):
        msg = BroadcastMessage.create(
            from_peer="infra",
            text="Deployment starting in 5 minutes",
        )

        assert msg.type == MessageType.BROADCAST
        assert msg.to_peer is None

    def test_message_serialization(self):
        msg = QueryMessage.create(
            from_peer="a",
            to_peer="b",
            text="hello",
        )

        data = msg.to_dict()
        restored = Message.from_dict(data)

        assert restored.id == msg.id
        assert restored.type == msg.type
        assert restored.from_peer == msg.from_peer
