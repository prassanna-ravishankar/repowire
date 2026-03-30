"""Unified peer registry: merges PeerManager + SessionMapper into one class.

Holds both the in-memory peer registry (_peers) and the persistent session
mappings (_mappings). Mutations that touch both stores happen under a single
lock, fixing the stale-mapping bug where set_peer_circle / update_peer_display_name
only updated the Peer but not the SessionMapping.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import time
from collections import deque
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import TYPE_CHECKING, Any
from uuid import uuid4

from repowire.config.models import DEFAULT_QUERY_TIMEOUT, AgentType, Config
from repowire.protocol.peers import Peer, PeerRole, PeerStatus

if TYPE_CHECKING:
    from repowire.daemon.message_router import MessageRouter
    from repowire.daemon.query_tracker import QueryTracker
    from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# SessionMapping dataclass (previously in session_mapper.py)
# ---------------------------------------------------------------------------

@dataclass
class SessionMapping:
    """Persistent mapping of session to peer identity."""

    session_id: str  # "repow-dev-a1b2c3d4"
    display_name: str
    circle: str
    backend: AgentType
    path: str | None = None
    role: PeerRole = PeerRole.AGENT
    updated_at: str | None = None

    def __post_init__(self) -> None:
        if self.updated_at is None:
            self.updated_at = datetime.now(timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# PeerRegistry
# ---------------------------------------------------------------------------

class PeerRegistry:
    """Unified peer registry with integrated session mapping.

    Combines the responsibilities of PeerManager (in-memory peer state,
    message routing delegation, event tracking) and SessionMapper (stable
    peer-ID allocation, disk persistence).

    Thread-safe with asyncio locks.
    """

    def __init__(
        self,
        config: Config,
        message_router: MessageRouter,
        query_tracker: QueryTracker | None = None,
        transport: WebSocketTransport | None = None,
        persistence_path: Path | None = None,
    ) -> None:
        self._config = config
        self._router = message_router
        self._query_tracker = query_tracker
        self._transport = transport

        # Peer registry: peer_id -> Peer (single source of truth)
        self._peers: dict[str, Peer] = {}

        # Session mappings: peer_id -> SessionMapping (persistent)
        self._mappings: dict[str, SessionMapping] = {}
        self._mappings_path = persistence_path or (
            Config.get_config_dir() / "sessions.json"
        )
        self._mappings_dirty = False
        self._load_mappings()

        self._lock = asyncio.Lock()
        self._events: deque[dict[str, Any]] = deque(maxlen=500)
        self._events_path = Config.get_config_dir() / "events.json"
        self._events_dirty = False
        self._load_events()
        self._last_repair: float = 0.0
        self._repair_lock = asyncio.Lock()

    # ------------------------------------------------------------------
    # Mapping persistence
    # ------------------------------------------------------------------

    def _load_mappings(self) -> None:
        """Load session mappings from disk."""
        if not self._mappings_path.exists():
            return
        try:
            data = json.loads(self._mappings_path.read_text())
            skipped = 0
            for session_id, mapping_data in data.items():
                path = mapping_data.get("path")
                if path and not Path(path).exists():
                    skipped += 1
                    continue
                self._mappings[session_id] = SessionMapping(**mapping_data)
            if skipped:
                logger.info(f"Skipped {skipped} mappings with non-existent paths")
                self._mappings_dirty = True
            logger.info(f"Loaded {len(self._mappings)} session mappings")
        except (json.JSONDecodeError, TypeError, ValueError, KeyError) as e:
            backup_ts = int(time.time())
            backup = self._mappings_path.with_suffix(f".json.corrupt.{backup_ts}")
            try:
                self._mappings_path.rename(backup)
                logger.error(f"Corrupt session mappings, backed up to {backup}: {e}")
            except OSError:
                logger.error(f"Corrupt session mappings (backup failed): {e}")
        except OSError as e:
            logger.error(f"Failed to read session mappings file: {e}")

    def _persist_mappings(self) -> None:
        """Save session mappings to disk atomically (debounced via dirty flag).

        Called from lazy_repair and shutdown, not on every mutation.
        """
        if not self._mappings_dirty:
            return
        tmp_path = self._mappings_path.with_suffix(".json.tmp")
        try:
            self._mappings_path.parent.mkdir(parents=True, exist_ok=True)
            data = {
                session_id: asdict(mapping)
                for session_id, mapping in self._mappings.items()
            }
            tmp_path.write_text(json.dumps(data, indent=2))
            os.replace(str(tmp_path), str(self._mappings_path))
            self._mappings_dirty = False
        except OSError as e:
            logger.error(f"Failed to save session mappings: {e}")
            try:
                tmp_path.unlink(missing_ok=True)
            except OSError:
                pass

    # ------------------------------------------------------------------
    # Event tracking
    # ------------------------------------------------------------------

    def _load_events(self) -> None:
        """Load persisted events from disk."""
        try:
            if self._events_path.exists():
                data = json.loads(self._events_path.read_text())
                for event in data[-100:]:
                    self._events.append(event)
        except Exception:
            logger.warning("Failed to load events from %s", self._events_path)

    def _save_events(self) -> None:
        """Persist events to disk (called periodically, not on every write)."""
        if not self._events_dirty:
            return
        try:
            self._events_path.parent.mkdir(parents=True, exist_ok=True)
            self._events_path.write_text(json.dumps(list(self._events)))
            self._events_dirty = False
        except Exception:
            logger.warning("Failed to save events to %s", self._events_path)

    def add_event(self, event_type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        event_id = str(uuid4())
        self._events.append(
            {
                "id": event_id,
                "type": event_type,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                **data,
            }
        )
        self._events_dirty = True
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

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def start(self) -> None:
        """Start the peer registry."""
        logger.info("PeerRegistry started with unified WebSocket backend")

    async def stop(self) -> None:
        """Stop the peer registry."""
        logger.info("PeerRegistry stopped")

    # ------------------------------------------------------------------
    # Peer lookup (internal, must hold _lock)
    # ------------------------------------------------------------------

    def _lookup_peer_unlocked(self, identifier: str, circle: str | None = None) -> Peer | None:
        """Lookup peer by session_id or display_name. Must be called with lock held.

        When multiple peers share a display_name (different circles), filters by
        circle if provided, otherwise prefers online ones.
        """
        if identifier in self._peers:
            return self._peers[identifier]
        # Scan all peers matching display_name
        matches = [p for p in self._peers.values() if p.display_name == identifier]
        if not matches:
            return None
        # Filter by circle if specified
        if circle:
            matches = [p for p in matches if p.circle == circle]
            if not matches:
                return None
        if len(matches) == 1:
            return matches[0]
        active = [p for p in matches if p.status != PeerStatus.OFFLINE]
        return active[0] if active else matches[0]

    @staticmethod
    def _sanitize_folder_name(name: str) -> str:
        """Sanitize a folder name for use in display_name.

        Replaces characters not matching [a-zA-Z0-9._-] with hyphens,
        collapses runs, strips leading/trailing hyphens.
        """
        sanitized = re.sub(r"[^a-zA-Z0-9._-]", "-", name)
        sanitized = re.sub(r"-{2,}", "-", sanitized)
        sanitized = sanitized.strip("-")
        return sanitized or "peer"

    def _build_display_name(
        self, path: str, circle: str, backend: AgentType,
    ) -> str:
        """Build a unique display_name for a peer. Must hold lock.

        Format: {folder}-{backend}[-{suffix}]
        Prunes offline peers that hold a conflicting name (clean takeover).
        """
        folder = self._sanitize_folder_name(Path(path).name) if path else "peer"
        base = f"{folder}-{backend.value}"

        candidate = base
        suffix = 2
        while True:
            blocker = None
            for sid, peer in self._peers.items():
                if peer.display_name == candidate and peer.circle == circle:
                    blocker = (sid, peer)
                    break

            if blocker is None:
                self._prune_name_from_mappings(candidate, circle, backend)
                return candidate

            sid, peer = blocker
            if peer.status == PeerStatus.OFFLINE:
                # Prune the offline peer entirely (clean takeover)
                del self._peers[sid]
                self._mappings.pop(sid, None)
                self._mappings_dirty = True
                logger.info(f"Pruned offline peer {candidate} ({sid}) for name reclaim")
                return candidate

            # Name held by an active peer -- try next suffix
            candidate = f"{folder}-{suffix}-{backend.value}"
            suffix += 1

    def _prune_name_from_mappings(
        self, display_name: str, circle: str, backend: AgentType,
    ) -> None:
        """Remove orphaned mappings for a name not held by any live peer. Must hold lock."""
        to_remove = [
            sid for sid, m in self._mappings.items()
            if m.display_name == display_name and m.circle == circle and m.backend == backend
            and sid not in self._peers
        ]
        for sid in to_remove:
            del self._mappings[sid]
            self._mappings_dirty = True

    def _find_or_allocate_mapping(
        self,
        display_name: str,
        circle: str,
        backend: AgentType,
        path: str | None = None,
        role: PeerRole = PeerRole.AGENT,
    ) -> str:
        """Find existing mapping or allocate a new session_id. Must hold lock.

        Returns the session_id (existing or new).
        """
        for sid, mapping in self._mappings.items():
            if (
                mapping.display_name == display_name
                and mapping.circle == circle
                and mapping.backend == backend
            ):
                mapping.path = path
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                logger.info(f"Reusing session {sid} for {display_name}@{circle}")
                self._mappings_dirty = True
                return sid

        session_id = f"repow-{circle}-{uuid4().hex[:8]}"
        self._mappings[session_id] = SessionMapping(
            session_id=session_id,
            display_name=display_name,
            circle=circle,
            backend=backend,
            path=path,
            role=role,
        )
        logger.info(f"Created session {session_id} for {display_name}@{circle}")
        self._mappings_dirty = True
        return session_id

    def _release_pane(self, pane_id: str, new_peer_id: str) -> None:
        """Clear pane_id from any peer that currently owns it, except new_peer_id.

        When a new ws-hook claims a pane, the old peer's pane registration is
        stale. Clearing it prevents get_peer_by_pane from returning the wrong
        peer after a session restart in the same tmux pane. Must hold lock.
        """
        for sid, peer in self._peers.items():
            if peer.pane_id == pane_id and sid != new_peer_id:
                peer.pane_id = None

    # ------------------------------------------------------------------
    # Allocate + register (atomic, the preferred public API)
    # ------------------------------------------------------------------

    async def allocate_and_register(
        self,
        *,
        circle: str,
        backend: AgentType,
        path: str | None = None,
        pane_id: str | None = None,
        tmux_session: str | None = None,
        metadata: dict | None = None,
        machine: str = "unknown",
        role: PeerRole = PeerRole.AGENT,
    ) -> tuple[str, str]:
        """Allocate a peer_id and register the peer atomically.

        Returns (peer_id, assigned_display_name). The daemon builds the
        display_name from path + backend, auto-suffixing on collision and
        pruning offline peers for clean name takeover.
        """
        async with self._lock:
            # Daemon owns the name: build it from path + backend
            assigned_name = self._build_display_name(path or "", circle, backend)
            peer_id = self._find_or_allocate_mapping(
                assigned_name, circle, backend, path, role=role,
            )
            if pane_id:
                self._release_pane(pane_id, peer_id)

            # --- create and insert Peer ---
            peer = Peer(
                peer_id=peer_id,
                display_name=assigned_name,
                circle=circle,
                backend=backend,
                role=role,
                status=PeerStatus.ONLINE,
                last_seen=datetime.now(timezone.utc),
                pane_id=pane_id,
                tmux_session=tmux_session,
                path=path or "",
                machine=machine,
                metadata=metadata or {},
            )
            self._peers[peer_id] = peer
            logger.info(f"Peer registered: {assigned_name} ({peer_id})")

            return peer_id, assigned_name

    # ------------------------------------------------------------------
    # register_peer (backward-compat for tests that build Peer objects)
    # ------------------------------------------------------------------

    async def register_peer(self, peer: Peer) -> None:
        """Register a pre-built Peer in the mesh.

        Indexed by peer_id. Evicts stale same-name peers but does NOT create
        or update session mappings -- use ``allocate_and_register`` for the
        full atomic path.
        """
        async with self._lock:
            # Evict offline peers with same (display_name, backend)
            for old_sid, old_peer in list(self._peers.items()):
                if (
                    old_peer.display_name == peer.display_name
                    and old_peer.backend == peer.backend
                    and old_sid != peer.peer_id
                    and (old_peer.circle == peer.circle or old_peer.status == PeerStatus.OFFLINE)
                ):
                    del self._peers[old_sid]
            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)
            self._peers[peer.peer_id] = peer
            logger.info(f"Peer registered: {peer.display_name} ({peer.peer_id})")

    # ------------------------------------------------------------------
    # Unregister
    # ------------------------------------------------------------------

    async def unregister_peer(self, identifier: str, circle: str | None = None) -> bool:
        """Unregister a peer from the mesh (removes from both _peers and _mappings).

        Args:
            identifier: Either session_id or display_name
            circle: Optional circle filter to disambiguate same-name peers

        Returns:
            True if peer was found and removed
        """
        async with self._lock:
            # Try as session_id first (always unambiguous)
            if identifier in self._peers:
                peer = self._peers.pop(identifier)
                self._mappings.pop(identifier, None)
                self._mappings_dirty = True
                logger.info(f"Peer unregistered: {peer.display_name} ({identifier})")
                return True

            # Try as display_name — with optional circle filter
            for sid, peer in list(self._peers.items()):
                if peer.display_name == identifier:
                    if circle and peer.circle != circle:
                        continue
                    self._peers.pop(sid)
                    self._mappings.pop(sid, None)
                    self._mappings_dirty = True
                    logger.info(f"Peer unregistered: {identifier} ({sid})")
                    return True

            return False

    # ------------------------------------------------------------------
    # Peer accessors
    # ------------------------------------------------------------------

    async def get_peer(self, identifier: str, circle: str | None = None) -> Peer | None:
        """Get a peer by session_id or display_name."""
        async with self._lock:
            return self._lookup_peer_unlocked(identifier, circle=circle)

    async def get_peer_by_pane(self, pane_id: str) -> Peer | None:
        """Lookup peer by tmux pane_id."""
        async with self._lock:
            for peer in self._peers.values():
                if peer.pane_id == pane_id:
                    return peer
            return None

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers."""
        async with self._lock:
            return list(self._peers.values())

    # ------------------------------------------------------------------
    # Circle access control (internal)
    # ------------------------------------------------------------------

    def _resolve_from_peer_unlocked(
        self, from_peer: str, target_peer: Peer, bypass_circle: bool
    ) -> Peer | None:
        """Resolve from_peer and check circle access. Must hold lock.

        Returns the resolved from_peer Peer object (or None if not found).
        """
        from_peer_obj = self._lookup_peer_unlocked(
            from_peer, circle=target_peer.circle
        ) or self._lookup_peer_unlocked(from_peer)
        self._check_circle_access_by_peers(from_peer_obj, target_peer, bypass_circle)
        return from_peer_obj

    def _check_circle_access_by_peers(
        self, from_obj: Peer | None, to_obj: Peer | None, bypass: bool
    ) -> None:
        """Check circle access given already-resolved Peer objects. Must hold lock."""
        if bypass:
            return
        if not from_obj or not to_obj:
            return
        if from_obj.bypasses_circles or to_obj.bypasses_circles:
            return
        if from_obj.circle != to_obj.circle:
            raise ValueError(
                f"Circle boundary: {from_obj.display_name} ({from_obj.circle}) "
                f"cannot access {to_obj.display_name} ({to_obj.circle})"
            )

    # ------------------------------------------------------------------
    # Message routing (query / notify / broadcast)
    # ------------------------------------------------------------------

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = DEFAULT_QUERY_TIMEOUT,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> str:
        """Send a query to a peer and wait for response.

        Raises:
            ValueError: If peer not found or circle boundary violated
            TimeoutError: If no response within timeout
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(to_peer, circle=circle)
            if not peer:
                raise ValueError(f"Unknown peer: {to_peer}")
            from_obj = self._resolve_from_peer_unlocked(from_peer, peer, bypass_circle)
            peer_id = peer.peer_id
            peer_name = peer.display_name
            from_peer_id = from_obj.peer_id if from_obj else None

        formatted_query = (
            f"[Repowire Query from @{from_peer}]\n"
            f"{text}\n\n"
            f"IMPORTANT: Respond directly in your message. Do NOT use ask_peer() to reply - "
            f"your response is automatically captured and returned to {from_peer}."
        )

        query_event_id = self.add_event(
            "query",
            {
                "from": from_peer, "to": to_peer, "text": text,
                "from_peer_id": from_peer_id, "to_peer_id": peer_id,
                "status": "pending",
            },
        )

        try:
            response = await self._router.send_query(
                from_peer=from_peer,
                to_session_id=peer_id,
                to_peer_name=peer_name,
                text=formatted_query,
                timeout=timeout,
            )

            self._update_event(query_event_id, {"status": "success"})
            self.add_event(
                "response",
                {
                    "from": to_peer, "to": from_peer,
                    "from_peer_id": peer_id, "to_peer_id": from_peer_id,
                    "text": response[:100] + "..." if len(response) > 100 else response,
                    "correlation_id": query_event_id,
                },
            )

            return response

        except TimeoutError:
            self._update_event(query_event_id, {"status": "timeout"})
            # Fire-and-forget liveness check — don't block the error path
            asyncio.ensure_future(self._check_peer_after_timeout(peer_id))
            raise

        except Exception as e:
            self._update_event(query_event_id, {"status": "error", "error": str(e)})
            raise

    async def _check_peer_after_timeout(self, peer_id: str) -> None:
        """Targeted liveness check after a query timeout. Runs in background."""
        if not self._transport or not self._transport.is_connected(peer_id):
            return
        try:
            await self._transport.ping(peer_id, timeout=5.0)
        except Exception:
            await self.update_peer_status(peer_id, PeerStatus.OFFLINE)
            if self._query_tracker:
                await self._query_tracker.cancel_queries_to_peer(peer_id)

    async def notify(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> None:
        """Send a notification to a peer (fire-and-forget).

        Raises:
            ValueError: If peer not found or circle boundary violated
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(to_peer, circle=circle)
            if not peer:
                raise ValueError(f"Unknown peer: {to_peer}")
            from_obj = self._resolve_from_peer_unlocked(from_peer, peer, bypass_circle)
            peer_id = peer.peer_id
            peer_name = peer.display_name
            from_peer_id = from_obj.peer_id if from_obj else None

        self.add_event(
            "notification",
            {
                "from": from_peer, "to": to_peer, "text": text,
                "from_peer_id": from_peer_id, "to_peer_id": peer_id,
            },
        )

        await self._router.send_notification(
            from_peer=from_peer,
            to_session_id=peer_id,
            to_peer_name=peer_name,
            text=text,
        )

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> list[str]:
        """Broadcast a message to all peers.

        Returns:
            List of peer names that received the broadcast
        """
        exclude_names = set(exclude or [])
        exclude_names.add(from_peer)

        exclude_session_ids: set[str] = set()
        async with self._lock:
            from_peer_obj: Peer | None = None
            for name in exclude_names:
                peer = self._lookup_peer_unlocked(name)
                if peer:
                    exclude_session_ids.add(peer.peer_id)
                    if name == from_peer:
                        from_peer_obj = peer

            sender_bypasses = from_peer_obj and (bypass_circle or from_peer_obj.bypasses_circles)
            if not sender_bypasses and from_peer_obj:
                from_circle = from_peer_obj.circle
                for sid, peer in self._peers.items():
                    if peer.circle != from_circle and not peer.bypasses_circles:
                        exclude_session_ids.add(sid)

            from_peer_id = from_peer_obj.peer_id if from_peer_obj else None
        self.add_event(
            "broadcast",
            {
                "from": from_peer, "text": text, "exclude": exclude,
                "from_peer_id": from_peer_id,
            },
        )

        sent_session_ids = await self._router.broadcast(
            from_peer=from_peer,
            text=text,
            exclude=exclude_session_ids,
        )

        async with self._lock:
            return [self._peers[sid].display_name for sid in sent_session_ids if sid in self._peers]

    # ------------------------------------------------------------------
    # Status / metadata mutations
    # ------------------------------------------------------------------

    async def update_peer_status(self, identifier: str, status: PeerStatus) -> None:
        """Update peer status."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                peer.status = status
                peer.last_seen = datetime.now(timezone.utc)
            else:
                logger.warning(
                    "update_peer_status: peer not found: %s (status=%s not applied)",
                    identifier,
                    status.value,
                )

    async def update_description(
        self, identifier: str, description: str, circle: str | None = None
    ) -> bool:
        """Update peer's task description."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier, circle=circle)
            if not peer:
                return False
            peer.description = description
            peer.last_seen = datetime.now(timezone.utc)
            return True

    async def set_peer_circle(self, identifier: str, circle: str) -> None:
        """Update peer's circle (both in-memory Peer AND persistent mapping)."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if peer:
                old_circle = peer.circle
                peer.circle = circle
                # Keep mapping in sync
                mapping = self._mappings.get(peer.peer_id)
                if mapping:
                    mapping.circle = circle
                    self._mappings_dirty = True
                logger.info(f"Peer {peer.display_name} moved from {old_circle} to {circle}")
            else:
                logger.warning(
                    "set_peer_circle: peer not found: %s (circle=%s not applied)",
                    identifier,
                    circle,
                )

    async def update_peer_display_name(self, session_id: str, new_name: str) -> bool:
        """Update a peer's display_name in-place, preserving peer_id.

        Evicts OFFLINE ghosts with the same (display_name, backend). Returns False
        if a conflicting ONLINE/BUSY peer exists with that name.

        Also updates the persistent mapping atomically.
        """
        async with self._lock:
            peer = self._peers.get(session_id)
            if not peer:
                return False
            to_evict = []
            for old_sid, old_peer in self._peers.items():
                if (
                    old_peer.display_name != new_name
                    or old_peer.backend != peer.backend
                    or old_sid == session_id
                ):
                    continue
                if old_peer.status == PeerStatus.OFFLINE:
                    to_evict.append(old_sid)
                else:
                    return False
            for old_sid in to_evict:
                del self._peers[old_sid]
            peer.display_name = new_name
            # Keep mapping in sync
            mapping = self._mappings.get(session_id)
            if mapping:
                mapping.display_name = new_name
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                self._mappings_dirty = True
            return True

    async def mark_offline(self, identifier: str) -> int:
        """Mark peer offline and cancel pending queries.

        Returns:
            Number of cancelled queries
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                return 0
            peer.status = PeerStatus.OFFLINE
            peer.last_seen = datetime.now(timezone.utc)
            session_id = peer.peer_id

        cancelled = 0
        if self._query_tracker:
            cancelled = await self._query_tracker.cancel_queries_to_peer(session_id)

        logger.info(f"Marked {identifier} offline, cancelled {cancelled} queries")
        return cancelled

    # ------------------------------------------------------------------
    # Session mapping helpers (public, formerly on SessionMapper)
    # ------------------------------------------------------------------

    def get_mapping(self, session_id: str) -> SessionMapping | None:
        """Get mapping for session_id."""
        return self._mappings.get(session_id)

    def get_all_mappings(self) -> dict[str, SessionMapping]:
        """Get all mappings."""
        return self._mappings.copy()

    def _register_session(
        self,
        display_name: str,
        circle: str,
        backend: AgentType,
        path: str | None = None,
    ) -> str:
        """Register or reuse session_id (synchronous, mapping-only, internal)."""
        return self._find_or_allocate_mapping(display_name, circle, backend, path)

    def _update_mapping_circle(self, session_id: str, circle: str) -> bool:
        """Update circle for an existing mapping (internal)."""
        mapping = self._mappings.get(session_id)
        if mapping:
            mapping.circle = circle
            self._mappings_dirty = True
            return True
        return False

    def _update_mapping_display_name(self, session_id: str, new_name: str) -> bool:
        """Update display_name for an existing mapping (internal)."""
        mapping = self._mappings.get(session_id)
        if mapping:
            mapping.display_name = new_name
            mapping.updated_at = datetime.now(timezone.utc).isoformat()
            self._mappings_dirty = True
            return True
        return False

    def _unregister_session(self, session_id: str) -> bool:
        """Unregister a single session mapping (internal)."""
        if session_id in self._mappings:
            del self._mappings[session_id]
            self._mappings_dirty = True
            logger.info(f"Unregistered session {session_id}")
            return True
        return False

    def _unregister_sessions(self, session_ids: list[str]) -> int:
        """Batch unregister session mappings (internal). Returns count removed."""
        removed = 0
        for sid in session_ids:
            if sid in self._mappings:
                del self._mappings[sid]
                removed += 1
        if removed:
            self._mappings_dirty = True
            logger.info(f"Batch unregistered {removed} sessions")
        return removed

    @staticmethod
    def _is_stale(mapping: SessionMapping, cutoff: datetime) -> bool:
        if not mapping.updated_at:
            return True
        try:
            return datetime.fromisoformat(mapping.updated_at) < cutoff
        except ValueError:
            return True

    def prune_offline(self, max_age_hours: float = 72) -> int:
        """Remove stale mappings older than max_age_hours.

        Returns:
            Number of pruned mappings.
        """
        cutoff = datetime.now(timezone.utc) - timedelta(hours=max_age_hours)
        initial_count = len(self._mappings)
        self._mappings = {
            sid: mapping
            for sid, mapping in self._mappings.items()
            if not self._is_stale(mapping, cutoff)
        }
        pruned_count = initial_count - len(self._mappings)

        if pruned_count > 0:
            self._mappings_dirty = True
            logger.info(
                f"Pruned {pruned_count} stale session mappings (>{max_age_hours}h old)"
            )

        return pruned_count

    # ------------------------------------------------------------------
    # Liveness repair
    # ------------------------------------------------------------------

    async def lazy_repair(self) -> None:
        """Debounced liveness sweep: ping ONLINE/BUSY peers, mark dead ones OFFLINE.

        Max 1x per 30s. Triggered by MCP-facing endpoints.
        """
        if time.monotonic() - self._last_repair < 30.0:
            return
        async with self._repair_lock:
            if time.monotonic() - self._last_repair < 30.0:
                return
            self._last_repair = time.monotonic()
            await self._do_repair()
            self._save_events()
            self._persist_mappings()

    async def _do_repair(self) -> None:
        """Actual repair logic. Must hold _repair_lock."""
        transport = self._transport
        if not transport:
            return

        async with self._lock:
            targets = [
                (p.peer_id, p.backend, p.circle)
                for p in self._peers.values()
                if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
            ]

        async def check_peer(
            peer_id: str, backend, circle: str,
        ) -> tuple[str, str | None] | None:
            """Returns (peer_id, circle) if alive, None if dead."""
            if not transport.is_connected(peer_id):
                return None
            if backend == AgentType.OPENCODE:
                return (peer_id, circle)
            try:
                pong = await transport.ping(peer_id, timeout=5.0)
                pong_circle = pong.get("circle")
                return (peer_id, pong_circle or circle)
            except Exception:
                return None

        results = await asyncio.gather(
            *(check_peer(pid, backend, circle) for pid, backend, circle in targets),
            return_exceptions=True,
        )

        alive_peers = [r for r in results if isinstance(r, tuple)]
        dead_peer_ids = {t[0] for t in targets} - {r[0] for r in alive_peers}

        # Circle recovery: update peers whose tmux session moved
        current_circles = {pid: c for pid, _, c in targets}
        for peer_id, new_circle in alive_peers:
            current = current_circles.get(peer_id)
            if current and new_circle and new_circle != current:
                logger.info(f"lazy_repair: circle recovery {peer_id}: {current} → {new_circle}")
                await self.set_peer_circle(peer_id, new_circle)

        for peer_id in dead_peer_ids:
            logger.info(f"lazy_repair: marking {peer_id} OFFLINE (no pong)")
            await self.update_peer_status(peer_id, PeerStatus.OFFLINE)
            if self._query_tracker:
                await self._query_tracker.cancel_queries_to_peer(peer_id)

        # Evict long-offline peers from BOTH _peers and _mappings
        max_age = self._config.daemon.prune_max_age_hours * 3600
        now = time.time()
        async with self._lock:
            stale = [
                pid for pid, p in self._peers.items()
                if p.status == PeerStatus.OFFLINE
                and p.last_seen
                and (now - p.last_seen.timestamp()) > max_age
            ]
            for pid in stale:
                del self._peers[pid]
                self._mappings.pop(pid, None)
            if stale:
                self._mappings_dirty = True
                logger.info(f"lazy_repair: evicted {len(stale)} stale offline peers")
