"""Core logic for the Repowire daemon."""

from __future__ import annotations

import asyncio
import logging
import socket
from collections import deque
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any
from uuid import uuid4

from repowire.config.models import Config, PeerConfig, load_config
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.backends.base import Backend
    from repowire.daemon.websocket_manager import WebSocketManager

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class SharedResources:
    """Shared resources for per-peer backend routing.

    This allows PeerManager to route messages to the appropriate backend
    based on peer connection type (WebSocket for OpenCode, tmux for claudemux).
    """

    ws_manager: WebSocketManager
    claudemux_backend: Backend | None = None
    opencode_backend: Backend | None = None

    def __post_init__(self) -> None:
        if self.claudemux_backend is None and self.opencode_backend is None:
            raise ValueError("At least one backend must be configured")


class PeerManager:
    """Manages peer registry and message routing.

    Thread-safe with asyncio locks. Delegates actual message
    delivery to the appropriate backend based on peer connection type.
    """

    def __init__(
        self,
        backend: Backend | None = None,
        config: Config | None = None,
        shared: SharedResources | None = None,
    ) -> None:
        """Initialize PeerManager.

        Args:
            backend: Legacy single-backend mode (deprecated, for backward compat)
            config: Configuration instance
            shared: Shared resources for per-peer routing (preferred)

        Raises:
            ValueError: If neither backend nor shared resources are provided
        """
        if backend is None and shared is None:
            raise ValueError("Either backend or shared resources must be provided")

        self._config = config or load_config()
        self._peers: dict[str, Peer] = {}
        self._lock = asyncio.Lock()
        self._machine = socket.gethostname()
        self._events: deque[dict[str, Any]] = deque(maxlen=100)

        # Per-peer routing with shared resources (preferred)
        self._shared = shared

        # Legacy single-backend mode (backward compatibility)
        self._backend = backend

    @property
    def backend_name(self) -> str:
        """Get the backend name (for health check / legacy)."""
        if self._backend:
            return self._backend.name
        # In per-peer mode, report what's available
        backends = []
        if self._shared and self._shared.claudemux_backend:
            backends.append("claudemux")
        if self._shared and self._shared.opencode_backend:
            backends.append("opencode")
        return "+".join(backends) if backends else "none"

    def _get_backend_for_peer(self, peer_config: PeerConfig) -> Backend:
        """Get appropriate backend for peer based on connection type.

        Args:
            peer_config: Peer configuration

        Returns:
            Backend instance to use for this peer

        Raises:
            ValueError: If no backend available for this peer
        """
        peer_name = peer_config.name

        # Per-peer routing mode (preferred)
        if self._shared:
            # Check WebSocket first (OpenCode peers)
            if self._shared.ws_manager.is_connected(peer_name):
                if self._shared.opencode_backend:
                    return self._shared.opencode_backend
                raise ValueError(
                    f"Peer {peer_name} connected via WebSocket but opencode backend unavailable"
                )

            # Check tmux session (claudemux peers)
            if peer_config.tmux_session:
                if self._shared.claudemux_backend:
                    return self._shared.claudemux_backend
                raise ValueError(
                    f"Peer {peer_name} has tmux session but claudemux backend unavailable"
                )

            raise ValueError(
                f"No backend for peer {peer_name}: not connected via WebSocket or tmux"
            )

        # Legacy single-backend mode
        if self._backend:
            return self._backend

        raise ValueError("No backend configured")

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
        """Start the peer manager and backend(s)."""
        if self._shared:
            if self._shared.claudemux_backend:
                await self._shared.claudemux_backend.start()
            if self._shared.opencode_backend:
                await self._shared.opencode_backend.start()
        elif self._backend:
            await self._backend.start()

    async def stop(self) -> None:
        """Stop the peer manager and backend(s)."""
        if self._shared:
            if self._shared.claudemux_backend:
                await self._shared.claudemux_backend.stop()
            if self._shared.opencode_backend:
                await self._shared.opencode_backend.stop()
        elif self._backend:
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

    def _get_peer_status(self, peer_config: PeerConfig) -> PeerStatus:
        """Get status of a peer from the appropriate backend.

        Args:
            peer_config: Peer configuration

        Returns:
            Peer status
        """
        # Per-peer routing mode
        if self._shared:
            # Check WebSocket first (OpenCode peers)
            if self._shared.ws_manager.is_connected(peer_config.name):
                return self._shared.ws_manager.get_peer_status(peer_config.name)

            # Check tmux session (claudemux peers)
            if peer_config.tmux_session and self._shared.claudemux_backend:
                return self._shared.claudemux_backend.get_peer_status(peer_config)

            return PeerStatus.OFFLINE

        # Legacy single-backend mode
        if self._backend:
            return self._backend.get_peer_status(peer_config)

        return PeerStatus.OFFLINE

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
            # Resolve circle for this peer
            circle = self.resolve_circle(peer_config)

            if peer_config.name in seen:
                # Use locally registered peer, but verify status with backend
                peer = local_peers[peer_config.name]
                backend_status = self._get_peer_status(peer_config)
                # If backend says offline but we think online/busy, trust backend
                if backend_status == PeerStatus.OFFLINE and peer.status != PeerStatus.OFFLINE:
                    peer.status = PeerStatus.OFFLINE
                # Always update circle from config/derivation
                peer.circle = circle
                result.append(peer)
            else:
                # Build peer from config
                status = self._get_peer_status(peer_config)
                result.append(
                    Peer(
                        name=peer_config.name,
                        path=peer_config.path or "",
                        machine=self._machine,
                        tmux_session=peer_config.tmux_session,
                        circle=circle,
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
                        path=peer_config.path or "",
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

    async def set_peer_circle(self, name: str, circle: str) -> bool:
        """Set a peer's circle for cross-backend communication.

        Updates both in-memory peer and persistent config atomically.

        Args:
            name: Name of the peer
            circle: Circle to join

        Returns:
            True if peer was found and updated
        """
        async with self._lock:
            updated = False

            # Update in-memory peer
            if name in self._peers:
                self._peers[name].circle = circle
                updated = True

            # Update config (persistent) - protected by lock to prevent race
            self._config = load_config()
            peer_config = self._config.get_peer(name)
            if peer_config:
                peer_config.circle = circle
                self._config.save()
                updated = True

            if updated:
                logger.info(f"Peer {name} joined circle: {circle}")
            else:
                logger.warning(f"Failed to update circle for {name}: peer not found")

            return updated

    async def update_peer_session_id(self, name: str, session_id: str) -> bool:
        """Update a peer's session ID.

        Updates persistent config atomically.

        Args:
            name: Name of the peer
            session_id: New session ID

        Returns:
            True if peer was found and updated
        """
        async with self._lock:
            self._config = load_config()
            peer_config = self._config.get_peer(name)
            if peer_config:
                peer_config.session_id = session_id
                self._config.save()
                return True
            logger.warning(f"Peer {name} not in config, session_id not persisted")
            return False

    async def register_peer_with_config(
        self,
        peer: Peer,
        path: str,
        opencode_url: str | None = None,
        circle: str | None = None,
    ) -> None:
        """Register a peer in both memory and config atomically.

        Args:
            peer: The peer to register
            path: Working directory path
            opencode_url: Optional OpenCode URL marker
            circle: Optional circle name
        """
        async with self._lock:
            # Update in-memory
            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)
            self._peers[peer.name] = peer

            # Update config (atomic with memory update)
            self._config = load_config()
            self._config.add_peer(
                name=peer.name,
                path=path,
                opencode_url=opencode_url,
                circle=circle,
            )

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

        # Cancel pending queries to this peer (check all available backends)
        cancelled = 0

        if self._shared:
            if self._shared.claudemux_backend:
                cancelled += await self._shared.claudemux_backend.cancel_queries_to_peer(name)
            if self._shared.opencode_backend:
                cancelled += await self._shared.opencode_backend.cancel_queries_to_peer(name)
        elif self._backend:
            cancelled = await self._backend.cancel_queries_to_peer(name)

        return cancelled

    def resolve_hook_response(self, correlation_id: str, response: str) -> bool:
        """Resolve a pending query with a response from a hook.

        This is called by the Stop hook (claudemux) to send back responses.

        Args:
            correlation_id: The correlation ID of the pending query
            response: The response text

        Returns:
            True if the query was found and resolved
        """
        # Try claudemux backend first (the only one that uses hooks)
        if self._shared and self._shared.claudemux_backend:
            return self._shared.claudemux_backend.resolve_query(correlation_id, response)
        elif self._backend:
            return self._backend.resolve_query(correlation_id, response)

        logger.warning(f"Cannot resolve hook response {correlation_id}: no backend available")
        return False

    def _get_peer_config(self, name: str) -> PeerConfig | None:
        """Get peer config by name."""
        self._config = load_config()
        return self._config.get_peer(name)

    def resolve_circle(self, peer_config: PeerConfig) -> str:
        """Resolve the circle for a peer.

        Priority: explicit config → backend derivation → global.

        Args:
            peer_config: The peer configuration

        Returns:
            Circle name
        """
        if peer_config.circle:
            return peer_config.circle

        # Per-peer routing mode - derive circle from appropriate backend
        if self._shared:
            # WebSocket peers (OpenCode) default to "global"
            if self._shared.ws_manager.is_connected(peer_config.name):
                return "global"

            # tmux peers (claudemux) derive from session name
            if peer_config.tmux_session and self._shared.claudemux_backend:
                return self._shared.claudemux_backend.derive_circle(peer_config)

            return "global"

        # Legacy single-backend mode
        if self._backend:
            return self._backend.derive_circle(peer_config)

        return "global"

    def _check_circle_access(self, from_peer: str, to_peer: str, bypass: bool = False) -> None:
        """Check if from_peer can communicate with to_peer.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            bypass: If True, skip circle check (CLI mode)

        Raises:
            ValueError: If peers are in different circles
        """
        # CLI always bypasses circle restrictions
        # CLI is the master - can communicate with any peer
        if bypass or from_peer == "cli":
            return

        from_cfg = self._get_peer_config(from_peer)
        if not from_cfg:
            raise ValueError(f"Sender peer '{from_peer}' is not registered")

        to_cfg = self._get_peer_config(to_peer)
        if not to_cfg:
            raise ValueError(f"Target peer '{to_peer}' is not registered")

        from_circle = self.resolve_circle(from_cfg)
        to_circle = self.resolve_circle(to_cfg)
        if from_circle != to_circle:
            raise ValueError(
                f"Circle boundary: {from_peer} (circle={from_circle}) cannot reach "
                f"{to_peer} (circle={to_circle})"
            )

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = 120.0,
        bypass_circle: bool = False,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds
            bypass_circle: If True, bypass circle restrictions (CLI mode)

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not found or circle boundary violated
            TimeoutError: If no response within timeout
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check circle access
        self._check_circle_access(from_peer, to_peer, bypass_circle)

        # Get appropriate backend for this peer (validates that peer is reachable)
        backend = self._get_backend_for_peer(peer_config)

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
            response = await backend.send_query(peer_config, formatted_query, timeout)
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

    async def notify(
        self, from_peer: str, to_peer: str, text: str, bypass_circle: bool = False
    ) -> bool:
        """Send a notification to a peer (fire-and-forget).

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Notification text
            bypass_circle: If True, bypass circle restrictions (CLI mode)

        Returns:
            True if message was sent successfully
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check circle access
        self._check_circle_access(from_peer, to_peer, bypass_circle)

        # Get appropriate backend for this peer (validates that peer is reachable)
        backend = self._get_backend_for_peer(peer_config)

        # Format the notification with sender info
        formatted_message = (
            f"[Repowire Notification from @{from_peer}]\n"
            f"{text}\n\n"
            f"To reply, use notify_peer and include the notification ID for correlation."
        )

        self._add_event("notification", {"from": from_peer, "to": to_peer, "text": text})

        try:
            await backend.send_message(peer_config, formatted_message)
            return True
        except Exception as e:
            logger.warning(f"Failed to send notification to {to_peer}: {e}")
            return False

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> list[str]:
        """Broadcast a message to all peers.

        Args:
            from_peer: Name of the sending peer
            text: Broadcast text
            exclude: Peer names to exclude
            bypass_circle: If True, send to all circles (CLI mode)

        Returns:
            List of peer names that received the message
        """
        excluded = set(exclude or [])
        excluded.add(from_peer)  # Don't send to self

        # Reload config
        self._config = load_config()
        sent_to: list[str] = []

        formatted_message = f"[Repowire Broadcast from @{from_peer}] {text}"

        self._add_event("broadcast", {"from": from_peer, "text": text})

        # Determine sender's circle for filtering (if not bypassing)
        sender_circle: str | None = None
        if not bypass_circle and from_peer != "cli":
            sender_config = self._get_peer_config(from_peer)
            if sender_config:
                sender_circle = self.resolve_circle(sender_config)

        for peer_config in self._config.peers.values():
            if peer_config.name in excluded:
                continue

            # Try to get backend for this peer (skips if not reachable)
            try:
                backend = self._get_backend_for_peer(peer_config)
            except ValueError:
                # Peer has no available backend, skip
                continue

            # Filter by circle (unless bypassing)
            if sender_circle is not None:
                peer_circle = self.resolve_circle(peer_config)
                if peer_circle != sender_circle:
                    continue

            # Check status via appropriate backend
            status = self._get_peer_status(peer_config)
            if status == PeerStatus.OFFLINE:
                continue

            try:
                await backend.send_message(peer_config, formatted_message)
                sent_to.append(peer_config.name)
            except Exception as e:
                logger.warning(f"Broadcast to {peer_config.name} failed: {e}")

        return sent_to
