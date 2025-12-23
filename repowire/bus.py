from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Callable, Coroutine
from uuid import uuid4


class MessageType(Enum):
    QUERY = "query"
    RESPONSE = "response"
    NOTIFICATION = "notification"
    BROADCAST = "broadcast"


@dataclass
class Message:
    id: str
    type: MessageType
    source: str
    target: str | None
    content: Any
    timestamp: datetime = field(default_factory=datetime.now)
    correlation_id: str | None = None

    @classmethod
    def query(cls, source: str, target: str, content: Any) -> Message:
        msg_id = str(uuid4())
        return cls(
            id=msg_id,
            type=MessageType.QUERY,
            source=source,
            target=target,
            content=content,
            correlation_id=msg_id,
        )

    @classmethod
    def response(cls, source: str, target: str, content: Any, correlation_id: str) -> Message:
        return cls(
            id=str(uuid4()),
            type=MessageType.RESPONSE,
            source=source,
            target=target,
            content=content,
            correlation_id=correlation_id,
        )

    @classmethod
    def notification(cls, source: str, target: str, content: Any) -> Message:
        return cls(
            id=str(uuid4()),
            type=MessageType.NOTIFICATION,
            source=source,
            target=target,
            content=content,
        )

    @classmethod
    def broadcast(cls, source: str, content: Any) -> Message:
        return cls(
            id=str(uuid4()),
            type=MessageType.BROADCAST,
            source=source,
            target=None,
            content=content,
        )


MessageHandler = Callable[[Message], Coroutine[Any, Any, Any]]


class MessageBus:
    def __init__(self) -> None:
        self._handlers: dict[str, MessageHandler] = {}
        self._pending_responses: dict[str, asyncio.Future[Message]] = {}
        self._message_log: list[Message] = []
        self._lock = asyncio.Lock()

    def register_handler(self, agent_name: str, handler: MessageHandler) -> None:
        self._handlers[agent_name] = handler

    def unregister_handler(self, agent_name: str) -> None:
        self._handlers.pop(agent_name, None)

    @property
    def registered_agents(self) -> list[str]:
        return list(self._handlers.keys())

    async def send(self, message: Message) -> None:
        async with self._lock:
            self._message_log.append(message)

        if message.type == MessageType.RESPONSE:
            if message.correlation_id and message.correlation_id in self._pending_responses:
                self._pending_responses[message.correlation_id].set_result(message)
            return

        if message.type == MessageType.BROADCAST:
            for agent_name, handler in self._handlers.items():
                if agent_name != message.source:
                    asyncio.create_task(handler(message))
            return

        if message.target and message.target in self._handlers:
            asyncio.create_task(self._handlers[message.target](message))

    async def query(self, message: Message, timeout: float = 30.0) -> Message:
        if message.type != MessageType.QUERY:
            raise ValueError("Only QUERY messages can await responses")

        future: asyncio.Future[Message] = asyncio.get_event_loop().create_future()
        self._pending_responses[message.correlation_id or message.id] = future

        try:
            await self.send(message)
            return await asyncio.wait_for(future, timeout=timeout)
        finally:
            self._pending_responses.pop(message.correlation_id or message.id, None)

    def get_message_log(self, limit: int = 100) -> list[Message]:
        return self._message_log[-limit:]
