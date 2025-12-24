from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import Literal


@dataclass
class Peer:
    name: str
    agent_type: Literal["happy", "opencode", "claude-code", "cursor"]
    session_id: str
    path: str
    capabilities: list[str] = field(default_factory=list)
    registered_at: datetime = field(default_factory=datetime.now)
    is_active: bool = True


class PeerRegistry:
    def __init__(self) -> None:
        self._peers: dict[str, Peer] = {}

    def register(self, peer: Peer) -> list[Peer]:
        self._peers[peer.name] = peer
        return list(self._peers.values())

    def unregister(self, name: str) -> bool:
        if name in self._peers:
            del self._peers[name]
            return True
        return False

    def get(self, name: str) -> Peer | None:
        return self._peers.get(name)

    def list_all(self) -> list[Peer]:
        return list(self._peers.values())

    def get_by_session_id(self, session_id: str) -> Peer | None:
        for peer in self._peers.values():
            if peer.session_id == session_id:
                return peer
        return None

    def set_active(self, name: str, is_active: bool) -> bool:
        peer = self._peers.get(name)
        if peer:
            peer.is_active = is_active
            return True
        return False

    @property
    def names(self) -> list[str]:
        return list(self._peers.keys())
