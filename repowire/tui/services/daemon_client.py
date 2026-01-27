"""HTTP client for daemon API."""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any, Literal

import httpx

from repowire.config.models import BackendType

logger = logging.getLogger(__name__)


@dataclass
class PeerInfo:
    """Peer information from daemon."""

    pane_id: str
    name: str  # Backward compat (= display_name)
    display_name: str
    status: str
    circle: str
    backend: BackendType
    path: str | None
    tmux_session: str | None
    opencode_url: str | None
    metadata: dict[str, Any]
    last_seen: str | None = None
    machine: str | None = None


@dataclass
class HealthInfo:
    """Daemon health information."""

    status: str
    version: str
    backend: str
    relay_mode: bool


@dataclass
class Event:
    """Communication event from daemon."""

    id: str
    type: str  # query, response, notification, broadcast, status_change
    timestamp: str
    # For communication events
    from_peer: str | None = None
    to_peer: str | None = None
    text: str = ""
    status: str | None = None
    query_id: str | None = None  # Links response to its query
    # For status_change events
    peer: str | None = None
    new_status: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Event:
        """Create from daemon event dict."""
        return cls(
            id=data.get("id", ""),
            type=data.get("type", "unknown"),
            timestamp=data.get("timestamp", ""),
            from_peer=data.get("from"),
            to_peer=data.get("to"),
            text=data.get("text", ""),
            status=data.get("status"),
            query_id=data.get("query_id"),
            peer=data.get("peer"),
            new_status=data.get("new_status"),
        )


@dataclass
class Conversation:
    """Query/response pair."""

    id: str
    from_peer: str
    to_peer: str
    query: Event
    response: Event | None
    timestamp: str
    status: Literal["pending", "success", "error"]

    @classmethod
    def from_events(cls, events: list[Event]) -> list[Conversation]:
        """Build conversations from event list."""
        queries = [e for e in events if e.type == "query"]
        responses = {e.query_id: e for e in events if e.type == "response" and e.query_id}

        convos = []
        for q in queries:
            resp = responses.get(q.id)
            status = "error" if q.status == "error" else ("success" if resp else "pending")
            convos.append(
                cls(
                    id=q.id,
                    from_peer=q.from_peer or "?",
                    to_peer=q.to_peer or "?",
                    query=q,
                    response=resp,
                    timestamp=q.timestamp,
                    status=status,
                )
            )
        return sorted(convos, key=lambda c: c.timestamp, reverse=True)


class DaemonClient:
    """HTTP client for repowire daemon API."""

    def __init__(self, base_url: str = "http://127.0.0.1:8377") -> None:
        self.base_url = base_url
        self._client: httpx.AsyncClient | None = None

    async def __aenter__(self) -> DaemonClient:
        self._client = httpx.AsyncClient(base_url=self.base_url, timeout=5.0)
        return self

    async def __aexit__(self, *args: Any) -> None:
        if self._client:
            await self._client.aclose()
            self._client = None

    @property
    def client(self) -> httpx.AsyncClient:
        if not self._client:
            raise RuntimeError("DaemonClient not initialized. Use async with.")
        return self._client

    async def health(self) -> HealthInfo | None:
        """Check daemon health."""
        try:
            resp = await self.client.get("/health")
            resp.raise_for_status()
            data = resp.json()
            return HealthInfo(
                status=data.get("status", "unknown"),
                version=data.get("version", "unknown"),
                backend=data.get("backend", "unknown"),
                relay_mode=data.get("relay_mode", False),
            )
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            logger.warning(f"Health check failed: {e}")
            return None

    async def get_peers(self) -> list[PeerInfo]:
        """Get list of all peers."""
        try:
            resp = await self.client.get("/peers")
            resp.raise_for_status()
            data = resp.json()
            return [
                PeerInfo(
                    pane_id=p.get("pane_id", f"legacy:{p.get('name', '?')}"),
                    name=p.get("name", "?"),
                    display_name=p.get("display_name", p.get("name", "?")),
                    status=p.get("status", "unknown"),
                    circle=p.get("circle", "global"),
                    backend=p.get("backend", "claudemux"),
                    path=p.get("path"),
                    tmux_session=p.get("tmux_session"),
                    opencode_url=p.get("opencode_url"),
                    metadata=p.get("metadata", {}),
                    last_seen=p.get("last_seen"),
                    machine=p.get("machine"),
                )
                for p in data.get("peers", [])
            ]
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            logger.warning(f"Failed to get peers: {e}")
            return []

    async def get_events(self) -> list[dict[str, Any]]:
        """Get communication events."""
        try:
            resp = await self.client.get("/events")
            resp.raise_for_status()
            data = resp.json()
            return data.get("events", [])
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            # Use debug level to avoid log spam from frequent polling
            logger.debug(f"Failed to get events: {e}")
            return []

    async def register_peer(
        self,
        name: str,
        path: str,
        tmux_session: str | None = None,
        opencode_url: str | None = None,
        circle: str | None = None,
        pane_id: str | None = None,
        display_name: str | None = None,
        backend: BackendType = "claudemux",
    ) -> bool:
        """Register a new peer."""
        try:
            resp = await self.client.post(
                "/peers",
                json={
                    "pane_id": pane_id,
                    "name": name,
                    "display_name": display_name or name,
                    "path": path,
                    "tmux_session": tmux_session,
                    "opencode_url": opencode_url,
                    "backend": backend,
                    "circle": circle,
                },
            )
            resp.raise_for_status()
            return True
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            logger.warning(f"Failed to register peer: {e}")
            return False

    async def unregister_peer(self, name: str) -> bool:
        """Unregister a peer."""
        try:
            resp = await self.client.delete(f"/peers/{name}")
            resp.raise_for_status()
            return True
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            logger.warning(f"Failed to unregister peer: {e}")
            return False

    async def set_peer_circle(self, name: str, circle: str) -> bool:
        """Set a peer's circle."""
        try:
            resp = await self.client.post(
                "/peers/circle",
                json={"peer_name": name, "circle": circle},
            )
            resp.raise_for_status()
            return True
        except (httpx.RequestError, httpx.HTTPStatusError) as e:
            logger.warning(f"Failed to set peer circle: {e}")
            return False
