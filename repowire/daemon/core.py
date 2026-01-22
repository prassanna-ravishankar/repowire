"""Core logic for the Repowire daemon."""

from __future__ import annotations

import asyncio
import logging
import socket
from collections import deque
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any
from uuid import uuid4

logger = logging.getLogger(__name__)

from repowire.config.models import Config, PeerConfig, load_config
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.backends.base import Backend


class PeerManager:
    """Manages peer registry and message routing.

    Thread-safe with asyncio locks. Delegates actual message
    delivery to the configured backend.
    """

    def __init__(self, backend: Backend, config: Config | None = None) -> None:
        self._backend = backend
        self._config = config or load_config()
        self._peers: dict[str, Peer] = {}
        self._lock = asyncio.Lock()
        self._machine = socket.gethostname()
        self._events: deque[dict[str, Any]] = deque(maxlen=100)

    @property
    def backend_name(self) -> str:
        """Get the backend name."""
        return self._backend.name

    def _add_event(self, type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        event_id = str(uuid4())
        self._events.append(
            {
                "id": event_id,
                "type": type,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                **data,
            }
        )
        return event_id

    def _update_event(self, event_id: str, updates: dict[str, Any]) -> bool:
        """Update an existing event by ID."""
        for event in self._events:
            if event["id"] == event_id:
                event.update(updates)
                return True
        return False

    def get_events(self) -> list[dict[str, Any]]:
        """Get the last 100 events."""
        return list(self._events)

    async def start(self) -> None:
        """Start the peer manager and backend."""
        await self._backend.start()

    async def stop(self) -> None:
        """Stop the peer manager and backend."""
        await self._backend.stop()

    async def register_peer(self, peer: Peer) -> None:
        """Register a peer in the mesh."""
        async with self._lock:
            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)
            self._peers[peer.name] = peer

    async def unregister_peer(self, name: str) -> bool:
        """Unregister a peer from the mesh."""
        async with self._lock:
            if name in self._peers:
                del self._peers[name]
                return True
            return False

    async def get_peer(self, name: str) -> Peer | None:
        """Get a peer by name."""
        async with self._lock:
            return self._peers.get(name)

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers."""
        # Reload config for fresh peer info
        self._config = load_config()

        async with self._lock:
            local_peers = {p.name: p for p in self._peers.values()}

        # Build peers from config with backend status
        result: list[Peer] = []
        seen = set(local_peers.keys())

        for peer_config in self._config.peers.values():
            if peer_config.name in seen:
                # Use locally registered peer, but verify status with backend
                peer = local_peers[peer_config.name]
                backend_status = self._backend.get_peer_status(peer_config)
                # If backend says offline but we think online/busy, trust backend
                if backend_status == PeerStatus.OFFLINE and peer.status != PeerStatus.OFFLINE:
                    peer.status = PeerStatus.OFFLINE
                result.append(peer)
            else:
                # Build peer from config
                status = self._backend.get_peer_status(peer_config)
                result.append(
                    Peer(
                        name=peer_config.name,
                        path=peer_config.path,
                        machine=self._machine,
                        tmux_session=peer_config.tmux_session,
                        status=status,
                        last_seen=datetime.now(timezone.utc)
                        if status != PeerStatus.OFFLINE
                        else None,
                        metadata=peer_config.metadata,
                    )
                )
                seen.add(peer_config.name)

        # Add any locally registered peers not in config
        for name, peer in local_peers.items():
            if name not in seen:
                result.append(peer)

        return result

    async def update_peer_status(self, name: str, status: PeerStatus) -> bool:
        """Update peer status."""
        async with self._lock:
            if name in self._peers:
                old_status = self._peers[name].status
                self._peers[name].status = status
                self._peers[name].last_seen = datetime.now(timezone.utc)
                # Log status change event if status actually changed
                if old_status != status:
                    self._add_event(
                        "status_change",
                        {
                            "peer": name,
                            "new_status": status.value,
                            "text": f"{name} is now {status.value}",
                        },
                    )
                return True
            else:
                # Peer not in memory yet - reload config and create from it
                self._config = load_config()
                peer_config = self._config.get_peer(name)
                if peer_config:
                    self._peers[name] = Peer(
                        name=name,
                        path=peer_config.path,
                        machine=self._machine,
                        tmux_session=peer_config.tmux_session,
                        status=status,
                        last_seen=datetime.now(timezone.utc),
                        metadata=peer_config.metadata,
                    )
                    self._add_event(
                        "status_change",
                        {
                            "peer": name,
                            "new_status": status.value,
                            "text": f"{name} is now {status.value}",
                        },
                    )
                    return True
            return False

    async def mark_offline(self, name: str) -> int:
        """Mark a peer as offline and cancel pending queries to it.

        Args:
            name: Name of the peer going offline

        Returns:
            Number of queries cancelled
        """
        # Update status
        async with self._lock:
            if name in self._peers:
                self._peers[name].status = PeerStatus.OFFLINE

        # Cancel pending queries to this peer
        cancelled = 0
        if hasattr(self._backend, "cancel_queries_to_peer"):
            cancelled = self._backend.cancel_queries_to_peer(name)

        return cancelled

    def _get_peer_config(self, name: str) -> PeerConfig | None:
        """Get peer config by name."""
        self._config = load_config()
        return self._config.get_peer(name)

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = 120.0,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not found
            TimeoutError: If no response within timeout
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check backend-specific requirements
        if self._backend.name == "claudemux" and not peer_config.tmux_session:
            raise ValueError(f"Peer {to_peer} has no tmux session (required for claudemux backend)")
        elif self._backend.name == "opencode" and not peer_config.opencode_url:
            raise ValueError(f"Peer {to_peer} has no opencode_url (required for opencode backend)")

        # Format the query with sender info and response instructions
        formatted_query = (
            f"[Repowire Query from @{from_peer}]\n"
            f"{text}\n\n"
            f"IMPORTANT: Respond directly in your message. Do NOT use ask_peer() to reply - "
            f"your response is automatically captured and returned to {from_peer}."
        )

        query_event_id = self._add_event(
            "query",
            {"from": from_peer, "to": to_peer, "text": text, "status": "pending"},
        )

        try:
            response = await self._backend.send_query(peer_config, formatted_query, timeout)
            # Update query event to success
            self._update_event(query_event_id, {"status": "success"})
            self._add_event(
                "response",
                {
                    "from": to_peer,
                    "to": from_peer,
                    "text": response,
                    "status": "success",
                    "query_id": query_event_id,
                },
            )
            return response
        except (ValueError, TimeoutError) as e:
            # Update query event to error for known error types
            self._update_event(query_event_id, {"status": "error", "error_message": str(e)})
            self._add_event(
                "response",
                {
                    "from": to_peer,
                    "to": from_peer,
                    "text": str(e),
                    "status": "error",
                    "query_id": query_event_id,
                },
            )
            raise
        except Exception as e:
            # Log unexpected errors and update events
            logger.exception(f"Unexpected error during query to {to_peer}")
            self._update_event(query_event_id, {"status": "error", "error_message": str(e)})
            self._add_event(
                "response",
                {
                    "from": to_peer,
                    "to": from_peer,
                    "text": str(e),
                    "status": "error",
                    "query_id": query_event_id,
                },
            )
            raise

    async def notify(self, from_peer: str, to_peer: str, text: str) -> bool:
        """Send a notification to a peer (fire-and-forget).

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Notification text

        Returns:
            True if message was sent successfully
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check backend-specific requirements
        if self._backend.name == "claudemux" and not peer_config.tmux_session:
            raise ValueError(f"Peer {to_peer} has no tmux session (required for claudemux backend)")
        elif self._backend.name == "opencode" and not peer_config.opencode_url:
            raise ValueError(f"Peer {to_peer} has no opencode_url (required for opencode backend)")

        # Format the notification with sender info
        formatted_message = f"[Repowire Notification from @{from_peer}] {text}"

        self._add_event("notification", {"from": from_peer, "to": to_peer, "text": text})

        try:
            await self._backend.send_message(peer_config, formatted_message)
            return True
        except Exception as e:
            logger.warning(f"Failed to send notification to {to_peer}: {e}")
            return False

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
    ) -> list[str]:
        """Broadcast a message to all peers.

        Args:
            from_peer: Name of the sending peer
            text: Broadcast text
            exclude: Peer names to exclude

        Returns:
            List of peer names that received the message
        """
        exclude = set(exclude or [])
        exclude.add(from_peer)  # Don't send to self

        # Reload config
        self._config = load_config()
        sent_to: list[str] = []

        formatted_message = f"[Repowire Broadcast from @{from_peer}] {text}"

        self._add_event("broadcast", {"from": from_peer, "text": text})

        for peer_config in self._config.peers.values():
            if peer_config.name in exclude:
                continue

            # Check backend-specific requirements
            if self._backend.name == "claudemux" and not peer_config.tmux_session:
                continue
            elif self._backend.name == "opencode" and not peer_config.opencode_url:
                continue

            status = self._backend.get_peer_status(peer_config)
            if status == PeerStatus.OFFLINE:
                continue

            try:
                await self._backend.send_message(peer_config, formatted_message)
                sent_to.append(peer_config.name)
            except Exception as e:
                logger.warning(f"Broadcast to {peer_config.name} failed: {e}")

        return sent_to
