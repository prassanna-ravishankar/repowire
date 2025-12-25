"""Message protocol definitions."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any
from uuid import uuid4

from pydantic import BaseModel, Field


class MessageType(str, Enum):
    """Type of message in the mesh."""

    QUERY = "query"  # Request that expects a response
    RESPONSE = "response"  # Response to a query
    NOTIFICATION = "notification"  # Fire-and-forget message
    BROADCAST = "broadcast"  # Message to all peers


class Message(BaseModel):
    """Base message in the Repowire mesh."""

    id: str = Field(default_factory=lambda: str(uuid4()), description="Unique message ID")
    type: MessageType = Field(..., description="Message type")
    from_peer: str = Field(..., description="Sender peer name")
    to_peer: str | None = Field(None, description="Target peer name (None for broadcast)")
    payload: dict[str, Any] = Field(..., description="Message payload")
    correlation_id: str | None = Field(None, description="For request/response matching")
    timestamp: datetime = Field(default_factory=datetime.utcnow, description="Message timestamp")

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for serialization."""
        return {
            "id": self.id,
            "type": self.type.value,
            "from_peer": self.from_peer,
            "to_peer": self.to_peer,
            "payload": self.payload,
            "correlation_id": self.correlation_id,
            "timestamp": self.timestamp.isoformat(),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Message:
        """Create from dictionary."""
        data = data.copy()
        if data.get("timestamp"):
            data["timestamp"] = datetime.fromisoformat(data["timestamp"])
        if data.get("type"):
            data["type"] = MessageType(data["type"])
        return cls(**data)


class QueryMessage(Message):
    """A query message that expects a response."""

    type: MessageType = Field(default=MessageType.QUERY)
    correlation_id: str = Field(default_factory=lambda: str(uuid4()))

    @classmethod
    def create(cls, from_peer: str, to_peer: str, text: str) -> QueryMessage:
        """Create a query message."""
        return cls(
            from_peer=from_peer,
            to_peer=to_peer,
            payload={"text": text},
        )


class ResponseMessage(Message):
    """A response to a query."""

    type: MessageType = Field(default=MessageType.RESPONSE)

    @classmethod
    def create(
        cls, from_peer: str, to_peer: str, text: str, correlation_id: str, success: bool = True
    ) -> ResponseMessage:
        """Create a response message."""
        return cls(
            from_peer=from_peer,
            to_peer=to_peer,
            payload={"text": text, "success": success},
            correlation_id=correlation_id,
        )


class NotificationMessage(Message):
    """A fire-and-forget notification."""

    type: MessageType = Field(default=MessageType.NOTIFICATION)

    @classmethod
    def create(cls, from_peer: str, to_peer: str, text: str) -> NotificationMessage:
        """Create a notification message."""
        return cls(
            from_peer=from_peer,
            to_peer=to_peer,
            payload={"text": text},
        )


class BroadcastMessage(Message):
    """A message to all peers."""

    type: MessageType = Field(default=MessageType.BROADCAST)
    to_peer: None = Field(default=None)

    @classmethod
    def create(cls, from_peer: str, text: str) -> BroadcastMessage:
        """Create a broadcast message."""
        return cls(
            from_peer=from_peer,
            payload={"text": text},
        )
