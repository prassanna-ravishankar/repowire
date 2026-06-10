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
from collections.abc import Awaitable, Callable
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import TYPE_CHECKING, Any, Literal
from uuid import uuid4

from repowire.agent_types import AgentType
from repowire.config.models import (
    DEFAULT_QUERY_TIMEOUT,
    Config,
    DaemonConfig,
    ExperimentsConfig,
)
from repowire.daemon import diagnostics as diag
from repowire.daemon.delivery_trace import DeliveryTraceStore
from repowire.daemon.event_log import EventLog
from repowire.daemon.registry_events import PeerContradictionTracker
from repowire.daemon.registry_identity import (
    CircleSource,
    SessionMapping,
    is_configured_orchestrator_path,
    normalize_identity_path,
)
from repowire.daemon.registry_repair import has_runtime_evidence, pid_alive
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.capabilities import PANE_UNSAFE_STRIKE_LIMIT
from repowire.protocol.peers import Peer, PeerRole, PeerStatus, TurnState

if TYPE_CHECKING:
    from repowire.daemon.ask_tracker import AskTracker
    from repowire.daemon.event_bus import EventBus
    from repowire.daemon.message_router import MessageRouter
    from repowire.daemon.query_tracker import QueryTracker
    from repowire.daemon.state import StateDatabase
    from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)

def _serialize_attachments(attachments: list | None) -> list[dict[str, Any]]:
    if not attachments:
        return []
    return [
        item.model_dump(exclude_none=True) if hasattr(item, "model_dump") else item
        for item in attachments
    ]


class PaneHijackRejectedError(Exception):
    """Raised by allocate_and_register when a fresh SessionStart claim is
    rejected because it appears to be a subprocess of the pane's existing
    agent (parent_pid matches the existing peer's agent_pid).
    """


class RoleClaimConflictError(Exception):
    """Raised when a live peer already holds the requested special role."""


class PeerRetiredError(Exception):
    """Raised by allocate_and_register when a claim names a retired peer_id
    without fresh runtime evidence (a live agent_pid). Stops orphan ws-hooks
    from resurrecting peers that were terminally marked offline.
    """


@dataclass
class RoleClaimResult:
    """Result of claiming a special peer role."""

    peer: Peer
    previous_holders: list[dict[str, str | None]]
    already_held: bool = False


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
        config: Config | None = None,
        message_router: MessageRouter | None = None,
        query_tracker: QueryTracker | None = None,
        transport: WebSocketTransport | None = None,
        persistence_path: Path | None = None,
        ask_tracker: AskTracker | None = None,
        event_bus: EventBus | None = None,
        event_log: EventLog | None = None,
        state_db: StateDatabase | None = None,
        *,
        daemon: DaemonConfig | None = None,
        experiments: ExperimentsConfig | None = None,
    ) -> None:
        # The registry only needs two config slices (daemon timings +
        # experiments), not the whole Config god-object. Accept either a full
        # Config (existing callers) or the slices directly (newer callers), and
        # store only the slices so the registry's dependency surface is narrow.
        if config is not None:
            daemon = daemon or config.daemon
            experiments = experiments or config.experiments
        self._daemon = daemon or DaemonConfig()
        self._experiments = experiments or ExperimentsConfig()
        if message_router is None:
            raise ValueError("PeerRegistry requires a message_router")
        self._router: MessageRouter = message_router
        self._query_tracker = query_tracker
        self._transport = transport
        self._ask_tracker = ask_tracker
        self._event_bus = event_bus
        self._event_log = event_log or EventLog()

        # Peer registry: peer_id -> Peer (single source of truth)
        self._peers: dict[str, Peer] = {}

        # Session mappings: peer_id -> SessionMapping (persistent)
        self._mappings: dict[str, SessionMapping] = {}
        self._mappings_path = persistence_path or (
            Config.get_config_dir() / "sessions.json"
        )
        self._mappings_dirty = False
        self._state_db = state_db
        self._sqlite_mappings_enabled = state_db is not None
        self._load_mappings()

        self._lock = asyncio.Lock()
        self._last_repair: float = 0.0
        self._repair_lock = asyncio.Lock()

        self._contradictions = PeerContradictionTracker()

        # Awaited on every terminal mark_offline (after the lock); wired by the
        # app to the fire-completion service so executor death fails in-flight
        # job fires. The registry stays job-agnostic.
        self._on_terminal_offline: (
            Callable[[str, str], Awaitable[None]] | None
        ) = None

        # When each peer's current description was set. Used for clear-on-read
        # TTL — see config.daemon.description_ttl_seconds. Registry-internal
        # so the wire-facing Peer schema stays untouched.
        self._description_set_at: dict[str, datetime] = {}

        # Terminally-offlined peers: peer_id -> retired-at. A reconnect claiming
        # a retired id is rejected unless it proves a live agent_pid; a real
        # SessionStart (which always carries one) clears the entry. Persisted
        # in the state DB so a daemon restart cannot hand an orphan hook one
        # free re-registration.
        self._retired: dict[str, datetime] = {}
        self._load_retired()

        # Consecutive honest pane_alive=false pongs per connected peer.
        self._pane_unsafe_strikes: dict[str, int] = {}

    def set_terminal_offline_hook(
        self, hook: Callable[[str, str], Awaitable[None]] | None,
    ) -> None:
        """Observe terminal offlines: ``await hook(peer_id, reason)``."""
        self._on_terminal_offline = hook

    def _load_retired(self) -> None:
        if self._state_db is None:
            return
        cutoff = datetime.now(timezone.utc) - timedelta(seconds=self._RETIRED_TTL_SECONDS)
        rows = self._state_db.conn.execute(
            "SELECT peer_id, retired_at FROM retired_peers",
        ).fetchall()
        expired: list[str] = []
        for row in rows:
            try:
                at = datetime.fromisoformat(row["retired_at"])
            except (TypeError, ValueError):
                expired.append(row["peer_id"])
                continue
            if at > cutoff:
                self._retired[row["peer_id"]] = at
            else:
                expired.append(row["peer_id"])
        if expired:
            with self._state_db.conn:
                self._state_db.conn.executemany(
                    "DELETE FROM retired_peers WHERE peer_id = ?",
                    [(peer_id,) for peer_id in expired],
                )

    def _retire(self, peer_id: str) -> None:
        """Record a terminal retirement (in-memory + durable)."""
        at = datetime.now(timezone.utc)
        self._retired[peer_id] = at
        if self._state_db is not None:
            with self._state_db.conn:
                self._state_db.conn.execute(
                    "INSERT OR REPLACE INTO retired_peers(peer_id, retired_at) VALUES (?, ?)",
                    (peer_id, at.isoformat()),
                )

    def _unretire(self, peer_id: str) -> None:
        """Clear a retirement after a registration with live runtime proof."""
        self._retired.pop(peer_id, None)
        if self._state_db is not None:
            with self._state_db.conn:
                self._state_db.conn.execute(
                    "DELETE FROM retired_peers WHERE peer_id = ?",
                    (peer_id,),
                )

    # ------------------------------------------------------------------
    # Mapping persistence
    # ------------------------------------------------------------------

    def _load_mappings(self) -> None:
        """Load session mappings from the configured persistence backend."""
        if self._sqlite_mappings_enabled:
            self._import_legacy_mappings_once()
            self._load_mappings_from_sqlite()
            return
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
        """Save session mappings to the configured backend.

        Called from lazy_repair and shutdown, not on every mutation.
        """
        if not self._mappings_dirty:
            return
        if self._sqlite_mappings_enabled:
            self._persist_mappings_to_sqlite()
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

    def _mapping_to_sql_params(self, mapping: SessionMapping) -> tuple[Any, ...]:
        backend = (
            mapping.backend.value
            if isinstance(mapping.backend, AgentType)
            else str(mapping.backend)
        )
        role = mapping.role.value if isinstance(mapping.role, PeerRole) else str(mapping.role)
        return (
            mapping.session_id,
            mapping.display_name,
            mapping.circle,
            backend,
            mapping.path,
            role,
            mapping.updated_at,
            mapping.description,
            mapping.model,
            mapping.agent_pid,
        )

    @staticmethod
    def _row_to_mapping(row: Any) -> SessionMapping:
        return SessionMapping(
            session_id=row["session_id"],
            display_name=row["display_name"],
            circle=row["circle"],
            backend=AgentType(row["backend"]),
            path=row["path"],
            role=PeerRole(row["role"]),
            updated_at=row["updated_at"],
            description=row["description"] or "",
            model=row["model"] if "model" in row.keys() else None,
            agent_pid=row["agent_pid"],
        )

    def _load_mappings_from_sqlite(self) -> None:
        if self._state_db is None:
            return
        rows = self._state_db.conn.execute(
            "SELECT * FROM peer_session_mappings",
        ).fetchall()
        skipped = 0
        self._mappings.clear()
        for row in rows:
            try:
                mapping = self._row_to_mapping(row)
            except (TypeError, ValueError, KeyError) as e:
                skipped += 1
                logger.warning("Skipping invalid SQLite session mapping row: %s", e)
                continue
            if mapping.path and not Path(mapping.path).exists():
                skipped += 1
                continue
            self._mappings[mapping.session_id] = mapping
        if skipped:
            logger.info("Skipped %d SQLite session mappings", skipped)
        logger.info("Loaded %d SQLite session mappings", len(self._mappings))

    def _persist_mappings_to_sqlite(self) -> None:
        if self._state_db is None:
            return
        try:
            with self._state_db.conn:
                self._state_db.conn.execute("DELETE FROM peer_session_mappings")
                self._state_db.conn.executemany(
                    """
                    INSERT INTO peer_session_mappings(
                        session_id, display_name, circle, backend, path, role,
                        updated_at, description, model, agent_pid
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    [self._mapping_to_sql_params(mapping) for mapping in self._mappings.values()],
                )
            self._mappings_dirty = False
        except Exception as e:  # noqa: BLE001
            logger.error("Failed to save SQLite session mappings: %s", e)

    def _import_legacy_mappings_once(self) -> None:
        """Import legacy sessions.json into SQLite once per source file."""
        if self._state_db is None or not self._mappings_path.exists():
            return
        source_path = str(self._mappings_path)
        stat = self._mappings_path.stat()
        imported = self._state_db.conn.execute(
            "SELECT 1 FROM legacy_imports WHERE source_path = ?",
            (source_path,),
        ).fetchone()
        if imported is not None:
            return

        row_count = 0
        status = "ok"
        error: str | None = None
        try:
            data = json.loads(self._mappings_path.read_text())
            if not isinstance(data, dict):
                raise ValueError("legacy sessions.json root must be an object")
            mappings: list[SessionMapping] = []
            for session_id, mapping_data in data.items():
                if not isinstance(mapping_data, dict):
                    continue
                payload = dict(mapping_data)
                payload.setdefault("session_id", session_id)
                mapping = SessionMapping(**payload)
                mappings.append(mapping)
            with self._state_db.conn:
                self._state_db.conn.executemany(
                    """
                    INSERT OR IGNORE INTO peer_session_mappings(
                        session_id, display_name, circle, backend, path, role,
                        updated_at, description, model, agent_pid
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    [self._mapping_to_sql_params(mapping) for mapping in mappings],
                )
                row_count = len(mappings)
                self._state_db.conn.execute(
                    """
                    INSERT INTO legacy_imports(
                        source_path, source_mtime, source_size, row_count, status, error
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (source_path, stat.st_mtime, stat.st_size, row_count, status, error),
                )
        except Exception as e:  # noqa: BLE001
            status = "error"
            error = str(e)
            with self._state_db.conn:
                self._state_db.conn.execute(
                    """
                    INSERT INTO legacy_imports(
                        source_path, source_mtime, source_size, row_count, status, error
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (source_path, stat.st_mtime, stat.st_size, row_count, status, error),
                )
            logger.error("Failed to import legacy session mappings from %s: %s", source_path, e)

    # ------------------------------------------------------------------
    # Event tracking
    # ------------------------------------------------------------------

    @property
    def _events(self) -> deque[dict[str, Any]]:
        return self._event_log.events

    @_events.setter
    def _events(self, events: deque[dict[str, Any]]) -> None:
        self._event_log.events = events

    @property
    def _events_path(self) -> Path:
        return self._event_log.path

    @_events_path.setter
    def _events_path(self, path: Path) -> None:
        self._event_log.path = path

    @property
    def _events_dirty(self) -> bool:
        return self._event_log.dirty

    @_events_dirty.setter
    def _events_dirty(self, dirty: bool) -> None:
        self._event_log.dirty = dirty

    @property
    def _event_subscribers(self) -> set[asyncio.Event]:
        return self._event_log.subscribers

    def _load_events(self) -> None:
        """Load persisted events from disk."""
        self._event_log.load()

    def _save_events(self) -> None:
        """Persist events to disk (called periodically, not on every write)."""
        self._event_log.save()

    def add_event(self, event_type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        return self._event_log.add_event(event_type, data)

    def _emit_contradiction(
        self, peer: Peer, code: str, severity: str, detail: str,
    ) -> None:
        """Emit a fail-loud ``peer_contradiction`` event, once per transition.

        Best-effort: a logging/event-store failure must never abort reconciliation.
        """
        self._contradictions.emit(peer, code, severity, detail, self.add_event)

    def _clear_contradiction(self, peer_id: str, code: str) -> None:
        """Forget a contradiction so a future recurrence re-emits once."""
        self._contradictions.clear(peer_id, code)

    def _clear_all_contradictions(self, peer_id: str) -> None:
        """Drop all contradiction state for a peer (e.g. on reap)."""
        self._contradictions.clear_all(peer_id)

    def subscribe_events(self) -> asyncio.Event:
        """Register a wakeup Event fired on each add_event call.

        Caller must pair with unsubscribe_events on cleanup.
        """
        return self._event_log.subscribe()

    def unsubscribe_events(self, evt: asyncio.Event) -> None:
        """Remove a subscriber Event registered via subscribe_events."""
        self._event_log.unsubscribe(evt)

    def events_since(self, event_id: str | None) -> list[dict[str, Any]]:
        """Return events after the given id. If id is None or evicted, return all."""
        return self._event_log.events_since(event_id)

    def _update_event(self, event_id: str, updates: dict[str, Any]) -> bool:
        """Update an existing event by ID."""
        return self._event_log.update_event(event_id, updates)

    def get_events(self) -> list[dict[str, Any]]:
        """Get the last 100 events."""
        return self._event_log.get_events()

    def _emit_status_change(
        self, peer: Peer, old_status: PeerStatus, new_status: PeerStatus,
    ) -> None:
        """Publish a PeerStatusChanged event if the status actually changed.

        Safe to call with the registry lock held: publish only schedules
        delivery tasks, it does not await.
        """
        if self._event_bus is None or old_status == new_status:
            return
        from repowire.daemon.event_bus import PeerStatusChanged
        self._event_bus.publish(
            PeerStatusChanged(
                peer_id=peer.peer_id,
                display_name=peer.display_name,
                old_status=old_status,
                new_status=new_status,
            )
        )

    def _emit_peer_offline_event(
        self,
        peer: Peer,
        old_status: PeerStatus,
        *,
        reason: str,
        source: str,
        detail: str | None = None,
        context: dict[str, Any] | None = None,
    ) -> None:
        """Persist why a peer became OFFLINE before reap can erase it."""
        if old_status == PeerStatus.OFFLINE:
            return
        data: dict[str, Any] = {
            "peer_id": peer.peer_id,
            "display_name": peer.display_name,
            "backend": peer.backend.value,
            "path": peer.path,
            "pane_id": peer.pane_id,
            "old_status": old_status.value,
            "new_status": PeerStatus.OFFLINE.value,
            "reason": reason,
            "source": source,
        }
        if detail:
            data["detail"] = detail
        if context:
            data["context"] = context
        try:
            self.add_event("peer_offline", data)
        except Exception:  # noqa: BLE001 - liveness repair must keep going
            logger.warning("Failed to emit peer_offline for %s", peer.peer_id, exc_info=True)

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
        circle if provided. If circle is unspecified and multiple *online*
        peers share the name, raises ValueError instead of silently picking
        one (see issue #136: misroute via ambiguous resolve).
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
        candidates = active or matches

        # Ambiguous: >1 viable candidate and no explicit circle to disambiguate.
        # Pane ownership is the only safe tiebreaker — a pane-owned peer is
        # locally bound to a live tmux pane, so picking it over a pane-less
        # twin is unambiguous. Anything else (e.g. last_seen) is a guess and
        # caused issue #136 misroutes. Refuse to guess in that case.
        if circle is None and len(candidates) > 1:
            paned = [p for p in candidates if p.pane_id]
            if len(paned) == 1:
                return paned[0]
            circles = sorted({p.circle for p in candidates})
            raise ValueError(
                f"Ambiguous peer name {identifier!r}: matches in circles "
                f"{circles}. Specify a circle= or pass a peer_id."
            )

        def preference(peer: Peer) -> tuple[bool, bool, float]:
            connected = bool(self._transport and self._transport.is_connected(peer.peer_id))
            last_seen = peer.last_seen.timestamp() if peer.last_seen else 0.0
            return connected, bool(peer.pane_id), last_seen

        return max(candidates, key=preference)

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

    @staticmethod
    def _runtime_session_id(metadata: dict[str, Any] | None) -> str | None:
        """Extract the runtime (hook) session id from a peer's metadata.

        Mirrors the route-layer extractor. Two peers cannot share a runtime
        session id (except transiently during a fork), so it is a stable
        reconnect identity for name reclaim.
        """
        if not metadata:
            return None
        for key in ("hook_session_id", "runtime_session_id", "session_id"):
            value = metadata.get(key)
            if isinstance(value, str) and value:
                return value
        return None

    def _build_display_name(
        self,
        path: str,
        circle: str,
        backend: AgentType,
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Build a unique display_name for a peer. Must hold lock.

        Format: {folder}-{backend}[-{suffix}]

        Reclaims (prunes) a conflicting peer when it is the *same logical peer*
        reconnecting, so a reconnect/daemon-restart keeps its name instead of
        churning a fresh -2/-3 suffix. "Same logical peer" is decided by, in
        order: (a) the runtime session id matches (two peers can't share one,
        except transiently during a fork — so an equal id IS this peer coming
        back, regardless of the blocker's status); (b) the blocker is OFFLINE
        (the original clean-takeover rule). Only a genuinely distinct, live peer
        on the same path+backend+circle gets a suffix.
        """
        folder = self._sanitize_folder_name(Path(path).name) if path else "peer"
        base = f"{folder}-{backend.value}"
        incoming_session = self._runtime_session_id(metadata)

        candidate = base
        suffix = 2
        while True:
            blocker = None
            for sid, peer in self._peers.items():
                if peer.display_name == candidate and peer.circle == circle:
                    blocker = (sid, peer)
                    break

            if blocker is None:
                return candidate

            sid, peer = blocker
            same_session = (
                incoming_session is not None
                and self._runtime_session_id(peer.metadata) == incoming_session
            )
            if same_session or peer.status == PeerStatus.OFFLINE:
                # Same logical peer reconnecting (matching runtime session) or a
                # dead peer holding the name -- reclaim it cleanly.
                del self._peers[sid]
                self._mappings.pop(sid, None)
                self._mappings_dirty = True
                logger.info(
                    "Reclaimed name %s from %s (%s) for %s peer (status=%s)",
                    candidate, peer.display_name, sid,
                    "reconnecting same-session" if same_session else "offline",
                    peer.status.value,
                )
                return candidate

            # Name held by a distinct live peer -- try next suffix
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
        *,
        model: str | None = None,
        role: PeerRole = PeerRole.AGENT,
        agent_pid: int | None = None,
        circle_source: CircleSource | None = None,
        preferred_session_id: str | None = None,
    ) -> str:
        """Find existing mapping or allocate a new session_id. Must hold lock.

        Returns the session_id (existing or new).
        """
        if preferred_session_id:
            mapping = self._mappings.get(preferred_session_id)
            if (
                mapping is not None
                and mapping.display_name == display_name
                and mapping.circle == circle
                and mapping.backend == backend
            ):
                mapping.path = path
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                if model is not None:
                    mapping.model = model
                if agent_pid is not None:
                    mapping.agent_pid = agent_pid
                self._mappings_dirty = True
                logger.info(
                    "Reusing preferred session %s for %s@%s",
                    preferred_session_id,
                    display_name,
                    circle,
                )
                return preferred_session_id

        for sid, mapping in self._mappings.items():
            if (
                mapping.display_name == display_name
                and mapping.circle == circle
                and mapping.backend == backend
            ):
                mapping.path = path
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                if agent_pid is not None:
                    mapping.agent_pid = agent_pid
                logger.info(f"Reusing session {sid} for {display_name}@{circle}")
                self._mappings_dirty = True
                return sid

        # Cross-circle adoption: when the caller supplied the fallback circle
        # ("default") because tmux context was missing, but a prior mapping
        # exists for the same (name, backend, path), reuse it so the peer's
        # prior circle and description survive restarts where the tmux session
        # name didn't propagate (e.g. claude --continue). Explicit provenance
        # matters: a real tmux session named "default" or a spawn hint for
        # "default" is intentional and must not be remapped to an old
        # non-default circle.
        can_cross_circle_adopt = circle_source in (None, "fallback")
        if circle == "default" and path and can_cross_circle_adopt:
            for sid, mapping in self._mappings.items():
                if (
                    mapping.display_name == display_name
                    and mapping.backend == backend
                    and mapping.path == path
                ):
                    mapping.updated_at = datetime.now(timezone.utc).isoformat()
                    if model is not None:
                        mapping.model = model
                    if agent_pid is not None:
                        mapping.agent_pid = agent_pid
                    self._mappings_dirty = True
                    logger.info(
                        f"Adopted prior session {sid} for {display_name} "
                        f"(restored circle={mapping.circle})"
                    )
                    return sid

        session_id = f"repow-{circle}-{uuid4().hex[:8]}"
        self._mappings[session_id] = SessionMapping(
            session_id=session_id,
            display_name=display_name,
            circle=circle,
            backend=backend,
            path=path,
            role=role,
            model=model,
            agent_pid=agent_pid,
        )
        logger.info(f"Created session {session_id} for {display_name}@{circle}")
        self._mappings_dirty = True
        return session_id

    def _is_fresh_orchestrator_pane(self, pane_id: str) -> Peer | None:
        """Return the live orchestrator peer holding ``pane_id``, or None.

        "Live" matches ``get_orchestrator`` semantics: role=ORCHESTRATOR,
        status ONLINE/BUSY, last_seen within heartbeat tolerance. Must hold
        lock. Used by ``allocate_and_register`` to make orchestrator pane
        ownership sticky against same-pane displacement by temporary peers.
        """
        tolerance = self.heartbeat_tolerance()
        now = datetime.now(timezone.utc)
        for peer in self._peers.values():
            if peer.pane_id != pane_id:
                continue
            if peer.role != PeerRole.ORCHESTRATOR:
                continue
            if peer.status not in (PeerStatus.ONLINE, PeerStatus.BUSY):
                continue
            if peer.last_seen is None:
                continue
            if (now - peer.last_seen).total_seconds() > tolerance:
                continue
            return peer
        return None

    def _release_pane(self, pane_id: str, new_peer_id: str) -> None:
        """Clear pane_id from any peer that currently owns it, except new_peer_id.

        When a new ws-hook claims a pane, the old peer's pane registration is
        stale. Clearing it prevents get_peer_by_pane from returning the wrong
        peer after a session restart in the same tmux pane. Must hold lock.

        Also marks the displaced peer offline: losing the pane means its
        ws-hook is no longer the live owner of that tmux pane, so the peer's
        inbound transport is gone. Leaving it ONLINE with pane_id=None creates
        zombie peers that future sessions may incorrectly claim.

        Exception: a fresh role=ORCHESTRATOR holder is never flipped OFFLINE
        or detached from the pane by this path. Pane ownership is sticky for
        the orchestrator; a temporary same-pane claimant must not silently
        demote or orphan the orchestrator's transport.
        """
        tolerance = self.heartbeat_tolerance()
        now = datetime.now(timezone.utc)
        for sid, peer in self._peers.items():
            if peer.pane_id != pane_id or sid == new_peer_id:
                continue
            is_fresh_orch = (
                peer.role == PeerRole.ORCHESTRATOR
                and peer.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and peer.last_seen is not None
                and (now - peer.last_seen).total_seconds() <= tolerance
            )
            if is_fresh_orch:
                logger.warning(
                    "_release_pane: preserving fresh orchestrator liveness "
                    "and pane ownership for %s (%s); status untouched",
                    peer.display_name,
                    peer.peer_id,
                )
                continue
            old_status = peer.status
            peer.pane_id = None
            peer.status = PeerStatus.OFFLINE
            self._emit_status_change(peer, old_status, PeerStatus.OFFLINE)
            self._emit_peer_offline_event(
                peer,
                old_status,
                reason="pane_displaced",
                source="allocate_and_register",
                detail=(
                    "A new ws-hook claimed this pane; the previous peer lost "
                    "pane ownership."
                ),
                context={"pane_id": pane_id, "new_peer_id": new_peer_id},
            )

    # ------------------------------------------------------------------
    # Allocate + register (atomic, the preferred public API)
    # ------------------------------------------------------------------

    async def allocate_and_register(
        self,
        *,
        circle: str,
        backend: AgentType,
        model: str | None = None,
        path: str | None = None,
        pane_id: str | None = None,
        tmux_session: str | None = None,
        metadata: dict | None = None,
        machine: str = "unknown",
        role: PeerRole = PeerRole.AGENT,
        peer_id: str | None = None,
        turn_state: TurnState | None = None,
        initial_status: PeerStatus = PeerStatus.ONLINE,
        agent_pid: int | None = None,
        parent_pid: int | None = None,
        circle_source: CircleSource | None = None,
        display_name_override: str | None = None,
    ) -> tuple[str, str]:
        """Allocate a peer_id and register the peer atomically.

        Returns (peer_id, assigned_display_name). The daemon builds the
        display_name from path + backend, auto-suffixing on collision and
        pruning offline peers for clean name takeover.

        If ``peer_id`` is provided and matches an existing peer, the peer is
        taken over in-place (WebSocket reconnect after HTTP pre-registration).
        """
        # Captured inside the lock, used outside it to schedule redelivery.
        result_peer_id: str | None = None
        result_name: str | None = None
        should_redeliver = False

        async with self._lock:
            # Retirement guard: a claim naming a terminally-offlined peer_id is
            # an orphan ws-hook reconnect unless it proves a live agent. Checked
            # against _retired (not _peers) so it also covers ids already
            # evicted from the registry.
            if peer_id and peer_id in self._retired:
                if agent_pid is None or not pid_alive(agent_pid):
                    raise PeerRetiredError(
                        f"peer_id {peer_id} was retired "
                        f"(agent_pid={agent_pid or 'absent'}); "
                        "re-register via a fresh SessionStart"
                    )
                self._unretire(peer_id)

            if peer_id is None and path and role == PeerRole.ORCHESTRATOR:
                folder = self._sanitize_folder_name(Path(path).name)
                expected_name = display_name_override or f"{folder}-{backend.value}"
                candidates = [
                    sid for sid, peer in self._peers.items()
                    if peer.display_name == expected_name
                    and peer.circle == circle
                    and peer.backend == backend
                    and peer.path == path
                    and peer.status == PeerStatus.OFFLINE
                ]
                if len(candidates) == 1:
                    peer_id = candidates[0]
                    logger.info(
                        "Reclaiming offline peer %s for %s@%s via "
                        "display_name/backend/path reconnect",
                        peer_id,
                        expected_name,
                        circle,
                    )

            # Reconnect: if caller provides a peer_id that exists, take over
            # only when it still describes the same peer identity. Stale pane
            # metadata can otherwise bind another pane's WebSocket to this id.
            if peer_id and peer_id in self._peers:
                existing = self._peers[peer_id]
                same_backend = existing.backend == backend
                same_path = not existing.path or not path or existing.path == path
                if not same_backend or not same_path:
                    logger.warning(
                        "Ignoring stale peer_id claim %s: existing=%s backend=%s path=%s "
                        "claim_backend=%s claim_path=%s",
                        peer_id,
                        existing.display_name,
                        existing.backend.value,
                        existing.path,
                        backend.value,
                        path,
                    )
                else:
                    old_status = existing.status
                    existing.status = initial_status
                    existing.last_seen = datetime.now(timezone.utc)
                    if turn_state is not None:
                        existing.turn_state = turn_state
                    if model is not None:
                        existing.model = model
                        mapping = self._mappings.get(peer_id)
                        if mapping is not None and mapping.model != model:
                            mapping.model = model
                            mapping.updated_at = datetime.now(timezone.utc).isoformat()
                            self._mappings_dirty = True
                    self._emit_status_change(existing, old_status, initial_status)
                    if pane_id:
                        self._release_pane(pane_id, peer_id)
                        existing.pane_id = pane_id
                    if tmux_session:
                        existing.tmux_session = tmux_session
                    if machine != "unknown":
                        existing.machine = machine
                    if agent_pid is not None:
                        existing.agent_pid = agent_pid
                        mapping = self._mappings.get(peer_id)
                        if mapping is not None and mapping.agent_pid != agent_pid:
                            mapping.agent_pid = agent_pid
                            self._mappings_dirty = True
                    if metadata:
                        # Merge fresh keys (e.g. ws-connect capabilities) onto the
                        # existing peer's metadata without dropping prior keys
                        # (e.g. project/branch from the HTTP SessionStart register).
                        existing.metadata = {**existing.metadata, **metadata}
                    logger.info(f"Peer reconnected: {existing.display_name} ({peer_id})")
                    result_peer_id = peer_id
                    result_name = existing.display_name
                    should_redeliver = True

            if result_peer_id is None:
                # Pane-hijack guard: a fresh SessionStart claim for a pane that
                # already has a live peer, where the new hook's parent_pid matches
                # the existing peer's agent_pid, is almost certainly a subprocess
                # agent (e.g. `gemini --yolo` run from inside a claude-code pane)
                # inheriting TMUX_PANE from its parent and trying to register on
                # the parent's pane. Reject — the original peer keeps the pane.
                if pane_id and parent_pid is not None:
                    tolerance = self.heartbeat_tolerance()
                    now = datetime.now(timezone.utc)
                    for existing in self._peers.values():
                        if existing.pane_id != pane_id:
                            continue
                        if existing.agent_pid is None or existing.agent_pid != parent_pid:
                            continue
                        if existing.last_seen is None:
                            continue
                        recently_seen = (
                            (now - existing.last_seen).total_seconds() <= tolerance
                        )
                        transport_connected = (
                            self._transport is not None
                            and self._transport.is_connected(existing.peer_id)
                        )
                        if not (recently_seen or transport_connected):
                            continue
                        logger.error(
                            "Rejecting pane-hijack SessionStart claim: "
                            "hook_pid=%s parent_pid=%s existing_agent_pid=%s "
                            "existing_peer_id=%s existing_display_name=%s pane_id=%s",
                            agent_pid,
                            parent_pid,
                            existing.agent_pid,
                            existing.peer_id,
                            existing.display_name,
                            pane_id,
                        )
                        raise PaneHijackRejectedError(
                            f"pane {pane_id} held by {existing.display_name} "
                            f"({existing.peer_id}); claimant parent_pid={parent_pid} "
                            f"matches existing agent_pid={existing.agent_pid}"
                        )

                # Sticky orchestrator pane: if pane_id is held by a fresh
                # role=ORCHESTRATOR peer and this isn't a same-id reconnect,
                # do not displace it. Register the new peer pane-less so its
                # outbound MCP/HTTP path still works, but pane bookkeeping
                # stays bound to the orchestrator. Without this a sibling-
                # shell SessionStart in the orchestrator workspace silently
                # tears down the orchestrator's transport and leaves it
                # unrecoverable when the temp peer exits.
                effective_pane_id = pane_id
                if pane_id:
                    sticky_holder = self._is_fresh_orchestrator_pane(pane_id)
                    if sticky_holder is not None:
                        same_sticky_identity = (
                            sticky_holder.backend == backend
                            and sticky_holder.circle == circle
                            and is_configured_orchestrator_path(sticky_holder.path)
                            and is_configured_orchestrator_path(path)
                        )
                        if same_sticky_identity:
                            old_status = sticky_holder.status
                            sticky_holder.status = initial_status
                            sticky_holder.last_seen = datetime.now(timezone.utc)
                            if turn_state is not None:
                                sticky_holder.turn_state = turn_state
                            if model is not None:
                                sticky_holder.model = model
                                mapping = self._mappings.get(sticky_holder.peer_id)
                                if mapping is not None and mapping.model != model:
                                    mapping.model = model
                                    mapping.updated_at = datetime.now(timezone.utc).isoformat()
                                    self._mappings_dirty = True
                            self._emit_status_change(
                                sticky_holder, old_status, initial_status
                            )
                            if machine != "unknown":
                                sticky_holder.machine = machine
                            if agent_pid is not None:
                                sticky_holder.agent_pid = agent_pid
                                mapping = self._mappings.get(sticky_holder.peer_id)
                                if (
                                    mapping is not None
                                    and mapping.agent_pid != agent_pid
                                ):
                                    mapping.agent_pid = agent_pid
                                    self._mappings_dirty = True
                            if metadata:
                                sticky_holder.metadata = {
                                    **sticky_holder.metadata,
                                    **metadata,
                                }
                            logger.info(
                                "Reusing sticky orchestrator identity %s (%s) "
                                "for same path/backend registration on pane %s",
                                sticky_holder.display_name,
                                sticky_holder.peer_id,
                                pane_id,
                            )
                            result_peer_id = sticky_holder.peer_id
                            result_name = sticky_holder.display_name
                            should_redeliver = True
                            if self._ask_tracker is not None:
                                asyncio.create_task(
                                    self._redeliver_pending_replies(
                                        sticky_holder.peer_id
                                    ),
                                    name=f"redeliver-{sticky_holder.peer_id[:12]}",
                                )
                            return sticky_holder.peer_id, sticky_holder.display_name
                        logger.warning(
                            "Sticky orchestrator pane: not displacing %s (%s) "
                            "on pane %s; new peer registers without pane ownership "
                            "(claim_path=%s claim_backend=%s)",
                            sticky_holder.display_name,
                            sticky_holder.peer_id,
                            pane_id,
                            path,
                            backend.value,
                        )
                        effective_pane_id = None

                preferred_mapping = self._mappings.get(peer_id) if peer_id else None
                if preferred_mapping is not None:
                    same_backend = preferred_mapping.backend == backend
                    same_path = (
                        not preferred_mapping.path
                        or not path
                        or preferred_mapping.path == path
                    )
                    if not same_backend or not same_path:
                        logger.warning(
                            "Ignoring stale persisted peer_id claim %s: "
                            "mapping_name=%s backend=%s path=%s claim_backend=%s "
                            "claim_path=%s",
                            peer_id,
                            preferred_mapping.display_name,
                            preferred_mapping.backend.value,
                            preferred_mapping.path,
                            backend.value,
                            path,
                        )
                        preferred_mapping = None

                # Fresh in-memory registration: daemon owns the name by
                # default. A known persisted mapping or runtime identity
                # certificate is daemon-minted proof of an existing peer
                # identity, so those rehydrations must not get a new collision
                # suffix or adopt another same-path peer's base mapping.
                assigned_name = (
                    display_name_override
                    or (preferred_mapping.display_name if preferred_mapping else None)
                    or self._build_display_name(path or "", circle, backend, metadata)
                )
                mapping_circle = preferred_mapping.circle if preferred_mapping else circle
                if display_name_override:
                    for existing_id, existing in list(self._peers.items()):
                        if (
                            existing_id != peer_id
                            and existing.display_name == assigned_name
                            and existing.circle == mapping_circle
                            and existing.backend == backend
                            and (
                                existing.status == PeerStatus.OFFLINE
                                or (
                                    effective_pane_id is not None
                                    and existing.pane_id == effective_pane_id
                                )
                            )
                        ):
                            del self._peers[existing_id]
                            self._mappings.pop(existing_id, None)
                            self._mappings_dirty = True
                            logger.info(
                                "Pruned conflicting peer %s (%s) for certified "
                                "identity rehydration as %s (%s)",
                                existing.display_name,
                                existing_id,
                                assigned_name,
                                peer_id,
                            )
                allocated_id = self._find_or_allocate_mapping(
                    assigned_name, mapping_circle, backend, path, model=model, role=role,
                    agent_pid=agent_pid, circle_source=circle_source,
                    preferred_session_id=peer_id,
                )
                if effective_pane_id:
                    self._release_pane(effective_pane_id, allocated_id)

                # Restore circle, role, and description from persisted mapping.
                # The mapping is the durable source of truth for these fields;
                # when _find_or_allocate_mapping adopted a prior session, its
                # stored circle/role may differ from the caller-supplied
                # defaults and should win.
                restored = self._mappings.get(allocated_id)
                effective_circle = restored.circle if restored else circle
                effective_role = restored.role if restored else role
                restored_description = restored.description if restored else ""
                restored_model = restored.model if restored else None
                # Caller-supplied agent_pid wins (it's the live process); fall
                # back to whatever the mapping persisted across daemon restart.
                effective_agent_pid = (
                    agent_pid
                    if agent_pid is not None
                    else (restored.agent_pid if restored else None)
                )
                # Preserve metadata from a prior registration of this peer
                # (e.g. branch/project set at SessionStart HTTP register) while
                # letting freshly-supplied keys (e.g. ws-connect capabilities)
                # win on overlap.
                prior_peer = self._peers.get(allocated_id)
                effective_metadata = dict(prior_peer.metadata) if prior_peer else {}
                if metadata:
                    effective_metadata.update(metadata)

                # --- create and insert Peer ---
                peer = Peer(
                    peer_id=allocated_id,
                    display_name=assigned_name,
                    circle=effective_circle,
                    backend=backend,
                    role=effective_role,
                    status=initial_status,
                    model=model or restored_model,
                    last_seen=datetime.now(timezone.utc),
                    pane_id=effective_pane_id,
                    tmux_session=tmux_session,
                    path=path or "",
                    machine=machine,
                    metadata=effective_metadata,
                    description=restored_description,
                    turn_state=turn_state,
                    agent_pid=effective_agent_pid,
                )
                self._peers[allocated_id] = peer
                # A fresh registration backed by a live agent reclaims a
                # retired identity (mapping reuse can hand back a retired id
                # without an explicit peer_id claim).
                if (
                    allocated_id in self._retired
                    and effective_agent_pid is not None
                    and pid_alive(effective_agent_pid)
                ):
                    self._unretire(allocated_id)
                logger.info(
                    "Peer registered: %s (%s, status=%s)",
                    assigned_name,
                    allocated_id,
                    initial_status.value,
                )
                result_peer_id = allocated_id
                result_name = assigned_name
                should_redeliver = True

        # Out of the lock. Schedule pass-2 (identity-tuple) redelivery for
        # both fresh allocations and same-id reconnects. The new peer is
        # already ONLINE, so update_peer_status won't fire — this is the
        # only redelivery hook on the SessionStart path. Pending-reply stashes
        # are now transport-neutral (ACP replies and structured question
        # answers both use them), so reconnect always gives the tracker a
        # chance to drain any existing stash.
        if should_redeliver and result_peer_id and self._ask_tracker is not None:
            asyncio.create_task(
                self._redeliver_pending_replies(result_peer_id),
                name=f"redeliver-{result_peer_id[:12]}",
            )

        assert result_peer_id is not None and result_name is not None
        return result_peer_id, result_name

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
            existing = self._peers.get(peer.peer_id)
            old_status = existing.status if existing else PeerStatus.OFFLINE
            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.now(timezone.utc)
            self._peers[peer.peer_id] = peer
            self._emit_status_change(peer, old_status, PeerStatus.ONLINE)
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
            peer = self._lookup_peer_unlocked(identifier, circle=circle)
            if peer:
                self._apply_description_ttl(peer)
            return peer

    async def resolve_peer_strict(
        self, identifier: str, circle: str | None = None
    ) -> Peer | list[Peer]:
        """Resolve a peer by id-or-display_name, returning all matches when ambiguous.

        Unlike `get_peer`, an ambiguous display_name does not silently pick a
        winner — the caller gets the full candidate list and must disambiguate.
        Use this for destructive operations (kill, etc.) where guessing is wrong.

        Returns:
            The matching `Peer` (single match or peer_id hit), an empty list if
            no peer matches, or a list of 2+ peers when the display_name is
            ambiguous after circle filtering.
        """
        async with self._lock:
            in_circle = lambda p: circle is None or p.circle == circle  # noqa: E731
            by_id = [p for p in self._peers.values() if p.peer_id == identifier and in_circle(p)]
            if by_id:
                self._apply_description_ttl(by_id[0])
                return by_id[0]
            by_name = [
                p for p in self._peers.values()
                if p.display_name == identifier and in_circle(p)
            ]
            if len(by_name) == 1:
                self._apply_description_ttl(by_name[0])
                return by_name[0]
            for p in by_name:
                self._apply_description_ttl(p)
            return by_name

    async def get_peer_by_pane(self, pane_id: str) -> Peer | None:
        """Lookup peer by tmux pane_id."""
        async with self._lock:
            for peer in self._peers.values():
                if peer.pane_id == pane_id:
                    self._apply_description_ttl(peer)
                    return peer
            return None

    async def get_peers_by_circle(self, circle: str) -> list[Peer]:
        """Get all peers in a given circle."""
        async with self._lock:
            peers = [p for p in self._peers.values() if p.circle == circle]
            for p in peers:
                self._apply_description_ttl(p)
            return peers

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers."""
        async with self._lock:
            peers = list(self._peers.values())
            for p in peers:
                self._apply_description_ttl(p)
            return peers

    def heartbeat_tolerance(self) -> int:
        """Seconds a peer's last_seen may lag before it's considered dead.

        Two heartbeat intervals: one missed beat is tolerated (normal jitter),
        two means the wire is dead. Public accessor so callers (routes, MCP)
        don't reach into `_config`.
        """
        return self._daemon.heartbeat_interval * 2

    async def get_orchestrator(self, circle: str) -> Peer | None:
        """Return the live orchestrator for a circle, or None.

        Live = role=ORCHESTRATOR, status in (ONLINE, BUSY), and last_seen within
        2 * heartbeat_interval (one missed beat tolerated, two means dead). If
        multiple match, returns the most recently seen.
        """
        tolerance = self.heartbeat_tolerance()
        now = datetime.now(timezone.utc)
        async with self._lock:
            candidates = [
                p for p in self._peers.values()
                if p.circle == circle
                and p.role == PeerRole.ORCHESTRATOR
                and p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and p.last_seen is not None
                and (now - p.last_seen).total_seconds() <= tolerance
            ]
            if not candidates:
                return None
            return max(candidates, key=lambda p: p.last_seen or now)

    async def is_orchestrator_present(self, circle: str) -> bool:
        """True iff a live orchestrator exists in the circle (see get_orchestrator)."""
        return (await self.get_orchestrator(circle)) is not None

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

    async def check_circle_access(
        self,
        from_obj: Peer | None,
        to_obj: Peer,
        *,
        bypass_circle: bool = False,
    ) -> None:
        """Public wrapper for the circle-access check.

        Use from non-WS dispatch paths (ACP broker routing) that need to
        enforce the same access semantics as the WS path without going
        through the full ``notify`` / ``deliver_ask`` pipeline.

        Raises ``ValueError`` on circle boundary violation.
        """
        async with self._lock:
            self._check_circle_access_by_peers(from_obj, to_obj, bypass_circle)

    async def check_access(
        self,
        *,
        from_peer: str,
        to_peer: str,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> tuple[Peer | None, Peer]:
        """Resolve sender/target and enforce circle access for a non-WS dispatch.

        Mirrors the lookup + circle-check that ``query``/``notify``/
        ``deliver_ask`` run inline before sending. Use from route handlers
        that dispatch outside the WS path (e.g. ACP-broker routing) so the
        same access semantics apply — circle boundaries, ambiguity 409s,
        and unknown-peer 404s should not depend on the transport.

        Returns ``(from_obj, target)``. ``from_obj`` is ``None`` if the
        sender display-name is unknown (matches existing notify behavior:
        unresolved senders log + proceed, they don't fail the call).

        Raises:
            ValueError: target unknown / circle boundary violation /
                ambiguous display-name lookup.
        """
        async with self._lock:
            target = self._lookup_peer_unlocked(to_peer, circle=circle)
            if not target:
                raise ValueError(f"Unknown peer: {to_peer}")
            from_obj = self._resolve_from_peer_unlocked(
                from_peer, target, bypass_circle,
            )
            return from_obj, target

    # ------------------------------------------------------------------
    # Message routing (query / notify / broadcast)
    # ------------------------------------------------------------------

    def _compat_delivery_service(self):
        """Build the WS-only delivery facade for legacy registry callers."""
        from repowire.daemon.peer_delivery import PeerDeliveryService
        from repowire.daemon.transport_router import PeerTransportRouter

        return PeerDeliveryService(
            registry=self,
            message_router=self._router,
            transport_router=PeerTransportRouter(
                experiments=self._experiments,
                registry=self,
                message_router=self._router,
            ),
            config=Config(daemon=self._daemon, experiments=self._experiments),
            ask_tracker=self._ask_tracker,
            session_binding_store=getattr(self, "_session_binding_store", None),
        )

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
        return await self._compat_delivery_service().query(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            timeout=timeout,
            bypass_circle=bypass_circle,
            circle=circle,
        )

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
        attachments: list | None = None,
    ) -> Literal["sent", "queued"]:
        """Send a notification to a peer (fire-and-forget).

        Direct wire send via the router. No daemon-side queueing: BUSY peers
        have a live WS and ws-hook buffers the tmux paste through the busy
        turn, so direct send is sufficient. Offline peers raise TransportError
        so callers can retry or escalate.

        Returns:
            "sent" if recipient was ONLINE at send-time (immediate delivery),
            "queued" if recipient was BUSY (ws-hook holds the paste until the
            current turn ends).

        Raises:
            ValueError: peer not found / circle boundary violated.
            TransportError: peer has no live WS.
        """
        return await self._compat_delivery_service().notify(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            bypass_circle=bypass_circle,
            circle=circle,
            attachments=attachments,
        )

    async def deliver_ask(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        correlation_id: str,
        reply_to: str | None = None,
        bypass_circle: bool = False,
        circle: str | None = None,
        attachments: list | None = None,
    ) -> None:
        """Inject an ask to a peer as a type=ask wire frame.

        Direct wire send via the router. BUSY isn't a delivery barrier under
        async ask semantics — ws-hook buffers the tmux paste through the busy
        turn. Offline peers raise TransportError so the /ask route can roll
        back the tracker registration and return an error to the caller.

        Raises:
            ValueError: peer not found / circle violation.
            TransportError: peer has no live WS.
        """
        await self._compat_delivery_service().deliver_ask(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            correlation_id=correlation_id,
            reply_to=reply_to,
            bypass_circle=bypass_circle,
            circle=circle,
            attachments=attachments,
        )

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> tuple[list[str], list[dict[str, str]]]:
        """Broadcast to all peers in sender's circle (or all, if sender bypasses).

        Best-effort per-peer: a stale WS for one recipient doesn't fail the
        whole call. Returns ([sent_to_names], [{peer, error}, ...]).
        """
        return await self._compat_delivery_service().broadcast(
            from_peer=from_peer,
            text=text,
            exclude=exclude,
            bypass_circle=bypass_circle,
        )

    # ------------------------------------------------------------------
    # Status / metadata mutations
    # ------------------------------------------------------------------

    async def update_peer_status(self, identifier: str, status: PeerStatus) -> None:
        """Update peer status + last_seen.

        When ``experiments.acp_broker_client`` is on, OFFLINE→ONLINE/BUSY
        transitions also schedule a background task that redelivers any
        ACP-stashed pending replies targeting this peer as the asker. The
        scheduling is gated on the flag so the default flag-off path has
        zero new behaviour or perf overhead vs. pre-phase-3: no extra task,
        no extra ask-tracker scan, no extra notify.
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                logger.warning(
                    "update_peer_status: peer not found: %s (status=%s not applied)",
                    identifier,
                    status.value,
                )
                return
            old_status = peer.status
            peer.status = status
            peer.last_seen = datetime.now(timezone.utc)
            self._emit_status_change(peer, old_status, status)
            if status == PeerStatus.OFFLINE:
                self._emit_peer_offline_event(
                    peer,
                    old_status,
                    reason="status_update_offline",
                    source="update_peer_status",
                    detail="Peer status was explicitly updated to offline.",
                )
            became_live = (
                old_status == PeerStatus.OFFLINE
                and status in (PeerStatus.ONLINE, PeerStatus.BUSY)
            )
            asker_peer_id = peer.peer_id if became_live else None

        if (
            asker_peer_id is not None
            and self._ask_tracker is not None
        ):
            asyncio.create_task(
                self._redeliver_pending_replies(asker_peer_id),
                name=f"redeliver-{asker_peer_id[:12]}",
            )

    async def _redeliver_pending_replies(self, asker_peer_id: str) -> None:
        """Drain stashed replies for an asker that just came back online.

        Two passes:
          1. ``take_pending_replies_for_asker`` — same peer_id reconnect
             (original behaviour).
          2. ``take_orphan_pending_replies_matching`` — full identity-tuple
             rebind for asks whose original ``from_peer_id`` is no longer in
             ``_peers`` (i.e. pruned by ``_build_display_name`` or reaped).
             Guarded by a uniqueness gate over live peers so an ambiguous
             tuple refuses rebind rather than misrouting.

        Best-effort: a failure here just leaves the reply stashed for the
        next reconnect / sweep. Successful redelivery closes open asks
        (ack_with_msg) and clears the stash so the Ask object isn't holding
        reply text it can never use again. Already answered structured
        questions stay closed as answered; only the redelivery stash is
        drained.
        """
        if self._ask_tracker is None:
            return
        # --- Pass 1: same peer_id reconnect ---
        try:
            pending = await self._ask_tracker.take_pending_replies_for_asker(asker_peer_id)
        except Exception as e:  # noqa: BLE001
            logger.warning("redeliver: snapshot failed for %s: %s", asker_peer_id, e)
            pending = []
        for ask in pending:
            await self._deliver_one_stashed(ask, asker_peer_id, rebind=False)

        # --- Pass 2: identity-tuple rebind ---
        async with self._lock:
            new_peer = self._peers.get(asker_peer_id)
            if new_peer is None:
                return
            if not (new_peer.display_name and new_peer.circle and new_peer.backend):
                return
            if not new_peer.machine or new_peer.machine == "unknown":
                return
            norm_path = normalize_identity_path(new_peer.path or "")
            if not norm_path:
                return
            tuple_fields = (
                new_peer.display_name,
                new_peer.circle,
                new_peer.backend.value,
                norm_path,
                new_peer.machine,
            )
            # Uniqueness gate: refuse rebind if any other live peer matches
            # the same full tuple. We compute matches under the lock so the
            # live-peers snapshot we pass to the tracker is consistent with
            # the gate decision.
            matches = [
                p for p in self._peers.values()
                if p.display_name == new_peer.display_name
                and p.circle == new_peer.circle
                and p.backend == new_peer.backend
                and (p.machine or "") == new_peer.machine
                and normalize_identity_path(p.path or "") == norm_path
            ]
            live_peer_ids = set(self._peers.keys())

        if len(matches) != 1 or matches[0].peer_id != asker_peer_id:
            logger.debug(
                "redeliver pass-2: ambiguous live tuple match for %s "
                "(%d candidates); refusing rebind",
                asker_peer_id, len(matches),
            )
            return

        try:
            orphans = await self._ask_tracker.take_orphan_pending_replies_matching(
                display_name=tuple_fields[0],
                circle=tuple_fields[1],
                backend=tuple_fields[2],
                path=tuple_fields[3],
                machine=tuple_fields[4],
                live_peer_ids=live_peer_ids,
            )
        except Exception as e:  # noqa: BLE001
            logger.warning(
                "redeliver pass-2: orphan scan failed for %s: %s",
                asker_peer_id, e,
            )
            return
        for ask in orphans:
            await self._deliver_one_stashed(ask, asker_peer_id, rebind=True)

    async def _deliver_one_stashed(
        self, ask: Any, asker_peer_id: str, *, rebind: bool,
    ) -> None:
        """Deliver a single stashed reply and mark it delivered on success.

        The Ask object is **never mutated** outside ``AskTracker`` locks.
        Notify targets ``asker_peer_id`` directly (the registry already
        knows the live id from its own pass-1/pass-2 caller); only on
        successful delivery do we ask the tracker to atomically rebind
        + clear (rebind path) or clear (same-id path), closing open asks in
        the same operation.
        On notify failure the stash is left exactly as we found it.
        """
        reply = ask.pending_reply
        if reply is None:
            return
        try:
            await self.notify(
                from_peer=ask.to_peer_id,
                to_peer=asker_peer_id,
                text=reply,
                bypass_circle=True,
            )
        except (ValueError, TransportError) as e:
            logger.info(
                "redeliver: %s still undeliverable to %s: %s",
                ask.correlation_id, asker_peer_id, e,
            )
            return
        if self._ask_tracker is None:
            return
        await self._ask_tracker.mark_pending_reply_delivered(
            ask.correlation_id,
            new_from_peer_id=asker_peer_id if rebind else None,
            reason="ack_with_msg",
        )
        logger.info(
            "redeliver%s: delivered stashed reply for %s to %s",
            " (rebound)" if rebind else "",
            ask.correlation_id, asker_peer_id,
        )

    async def update_peer_turn_state(
        self, identifier: str, turn_state: TurnState | None,
    ) -> None:
        """Update peer turn_state (idle/working/awaiting_input/pending_first_turn).

        Orthogonal to status; does not refresh last_seen on its own (the
        accompanying status update typically does). No event is emitted for
        v1 — list_peers consumers pick it up on next read.
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                logger.warning(
                    "update_peer_turn_state: peer not found: %s (turn_state=%s not applied)",
                    identifier,
                    turn_state,
                )
                return
            peer.turn_state = turn_state

    async def touch_last_seen(
        self, identifier: str, circle: str | None = None,
    ) -> bool:
        """Refresh a peer's last_seen without changing transport status.

        Outbound MCP traffic from a peer is process activity, not proof that
        inbound delivery is reachable. WebSocket connect/disconnect owns
        ONLINE/OFFLINE; touch only feeds last_seen-keyed liveness checks.
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier, circle=circle)
            if not peer:
                return False
            peer.last_seen = datetime.now(timezone.utc)
        return True

    async def update_peer_model(
        self,
        identifier: str,
        model: str,
        circle: str | None = None,
    ) -> None:
        """Update peer's observed runtime model in live and durable state."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier, circle=circle)
            if not peer:
                logger.warning(
                    "update_peer_model: peer not found: %s (model=%s not applied)",
                    identifier,
                    model,
                )
                return
            if peer.model == model:
                return
            peer.model = model
            peer.last_seen = datetime.now(timezone.utc)
            mapping = self._mappings.get(peer.peer_id)
            if mapping and mapping.model != model:
                mapping.model = model
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                self._mappings_dirty = True

    async def update_peer_metadata(
        self,
        identifier: str,
        metadata: dict[str, Any],
        circle: str | None = None,
    ) -> None:
        """Merge peer metadata into live state."""
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier, circle=circle)
            if not peer:
                logger.warning(
                    "update_peer_metadata: peer not found: %s (metadata not applied)",
                    identifier,
                )
                return
            peer.metadata = {**peer.metadata, **metadata}
            peer.last_seen = datetime.now(timezone.utc)

    async def claim_special_role(
        self,
        identifier: str,
        role: PeerRole,
        *,
        circle: str | None = None,
        force: bool = False,
    ) -> RoleClaimResult | None:
        """Claim a singleton special role for an existing live peer.

        Narrow v0.13 repair hook: only orchestrator is supported. The target
        peer must already exist; this never allocates a new peer. A fresh
        ONLINE/BUSY holder in the same circle always blocks the claim. Offline
        or stale holders are demoted in both live registry state and durable
        mappings so restarts do not reintroduce the bad role.
        """
        if role != PeerRole.ORCHESTRATOR:
            raise ValueError("Only role=orchestrator can be claimed")

        now = datetime.now(timezone.utc)
        tolerance = self.heartbeat_tolerance()

        def fresh_holder(peer: Peer) -> bool:
            return (
                peer.role == role
                and peer.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and peer.last_seen is not None
                and (now - peer.last_seen).total_seconds() <= tolerance
            )

        async with self._lock:
            target = self._lookup_peer_unlocked(identifier, circle=circle)
            if not target:
                return None
            target_circle = circle or target.circle

            blockers = [
                p for p in self._peers.values()
                if p.peer_id != target.peer_id
                and p.circle == target_circle
                and fresh_holder(p)
            ]
            if blockers:
                holder = max(blockers, key=lambda p: p.last_seen or now)
                force_note = (
                    "; force cannot demote a fresh live orchestrator"
                    if force else ""
                )
                raise RoleClaimConflictError(
                    f"role={role.value} is already held by "
                    f"{holder.display_name} ({holder.peer_id}) in circle {target_circle}"
                    f"{force_note}"
                )

            previous_holders: list[dict[str, str | None]] = []
            for peer in self._peers.values():
                if (
                    peer.peer_id == target.peer_id
                    or peer.circle != target_circle
                    or peer.role != role
                ):
                    continue
                previous_holders.append(
                    {
                        "peer_id": peer.peer_id,
                        "display_name": peer.display_name,
                        "status": peer.status.value,
                        "last_seen": peer.last_seen.isoformat() if peer.last_seen else None,
                    }
                )
                peer.role = PeerRole.AGENT
                mapping = self._mappings.get(peer.peer_id)
                if mapping and mapping.role != PeerRole.AGENT:
                    mapping.role = PeerRole.AGENT
                    mapping.updated_at = now.isoformat()
                    self._mappings_dirty = True

            already_held = target.role == role
            target.role = role
            target.last_seen = now
            mapping = self._mappings.get(target.peer_id)
            if mapping:
                mapping.role = role
                mapping.updated_at = now.isoformat()
                self._mappings_dirty = True

            for sid, mapping in self._mappings.items():
                if (
                    sid == target.peer_id
                    or mapping.circle != target_circle
                    or mapping.role != role
                ):
                    continue
                previous_holders.append(
                    {
                        "peer_id": sid,
                        "display_name": mapping.display_name,
                        "status": "mapping-only",
                        "last_seen": mapping.updated_at,
                    }
                )
                mapping.role = PeerRole.AGENT
                mapping.updated_at = now.isoformat()
                self._mappings_dirty = True

            self.add_event(
                "role_claimed",
                {
                    "peer_id": target.peer_id,
                    "peer": target.display_name,
                    "role": role.value,
                    "circle": target_circle,
                    "force": force,
                    "previous_holders": previous_holders,
                },
            )
            return RoleClaimResult(
                peer=target,
                previous_holders=previous_holders,
                already_held=already_held,
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
            if description:
                self._description_set_at[peer.peer_id] = datetime.now(timezone.utc)
            else:
                self._description_set_at.pop(peer.peer_id, None)
            mapping = self._mappings.get(peer.peer_id)
            if mapping and mapping.description != description:
                mapping.description = description
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                self._mappings_dirty = True
            return True

    def _apply_description_ttl(self, peer: Peer) -> None:
        """Clear peer.description if it's older than the configured TTL.

        Mutates in place so list_peers / get_peer reflect cleared state on the
        next read without waiting for a sweep. TTL <= 0 disables the check.
        """
        if not peer.description:
            return
        ttl = self._daemon.description_ttl_seconds
        if ttl <= 0:
            return
        set_at = self._description_set_at.get(peer.peer_id)
        if set_at is None:
            # Description was set before TTL tracking started (e.g. restored
            # from a persisted mapping). Stamp now so the TTL window is
            # well-defined, rather than clearing a description we cannot age.
            self._description_set_at[peer.peer_id] = datetime.now(timezone.utc)
            return
        if (datetime.now(timezone.utc) - set_at).total_seconds() < ttl:
            return
        peer.description = ""
        self._description_set_at.pop(peer.peer_id, None)
        mapping = self._mappings.get(peer.peer_id)
        if mapping and mapping.description:
            mapping.description = ""
            mapping.updated_at = datetime.now(timezone.utc).isoformat()
            self._mappings_dirty = True

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

    async def mark_offline(
        self,
        identifier: str,
        *,
        reason: str = "mark_offline",
        source: str = "peer_registry",
        detail: str | None = None,
        context: dict[str, Any] | None = None,
        terminal: bool = False,
    ) -> int:
        """Mark peer offline and cancel pending queries.

        ``terminal=True`` means the caller knows the agent behind this peer is
        gone (SessionEnd, agent pid exited, pane takeover, 3-strike pane loss).
        The peer is retired — its transport is severed and a later reconnect
        claiming this peer_id is rejected unless it proves a live agent_pid —
        so an orphan ws-hook cannot resurrect it.

        Returns:
            Number of cancelled queries
        """
        async with self._lock:
            peer = self._lookup_peer_unlocked(identifier)
            if not peer:
                # Terminal offline for an id already evicted from the registry
                # must still retire it, or the orphan it came from could
                # re-register through a persisted mapping.
                if terminal and identifier.startswith("repow-"):
                    self._retire(identifier)
                return 0
            old_status = peer.status
            peer.status = PeerStatus.OFFLINE
            peer.last_seen = datetime.now(timezone.utc)
            self._emit_status_change(peer, old_status, PeerStatus.OFFLINE)
            self._emit_peer_offline_event(
                peer,
                old_status,
                reason=reason,
                source=source,
                detail=detail,
                context=context,
            )
            session_id = peer.peer_id
            doomed_ws = None
            if terminal:
                self._retire(session_id)
                if self._transport:
                    doomed_ws = self._transport.current_websocket(session_id)

        if terminal and self._transport and doomed_ws is not None:
            # Identity-checked: a legitimate reconnect that reclaimed this id
            # after we released the lock must not lose its fresh socket.
            removed = await self._transport.disconnect(session_id, doomed_ws)
            if removed:
                # Popping the registry alone leaves the TCP connection open
                # and the hook none the wiser — close it so the hook's
                # reconnect hits the retirement guard and exits (releasing
                # its pane flock). Best-effort: the socket may already be gone.
                try:
                    await doomed_ws.close(code=4004, reason="peer retired")
                except Exception:
                    pass

        cancelled = 0
        if self._query_tracker:
            cancelled = await self._query_tracker.cancel_queries_to_peer(session_id)

        if terminal and self._on_terminal_offline is not None:
            # Fire-completion seam: a terminal offline means the agent behind
            # the peer is conclusively gone, so in-flight job fires assigned to
            # it must fail loudly instead of staying delivered/running forever.
            try:
                await self._on_terminal_offline(session_id, reason)
            except Exception:
                logger.exception("terminal-offline hook failed for %s", session_id)

        logger.info(
            "Marked %s offline (reason=%s source=%s), cancelled %d queries",
            identifier,
            reason,
            source,
            cancelled,
        )
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
    # Maintenance
    # ------------------------------------------------------------------

    async def lazy_repair(self) -> None:
        """Debounced maintenance: demote ghosts, evict stale, persist.

        Max 1x per 30s. Called from message/peer endpoints. Lifecycle hooks
        and WebSocket disconnect handle liveness — this catches stragglers.
        """
        if time.monotonic() - self._last_repair < 30.0:
            return
        async with self._repair_lock:
            if time.monotonic() - self._last_repair < 30.0:
                return
            self._last_repair = time.monotonic()
            await self._demote_disconnected_peers()
            await self._demote_unsafe_connected_peers()
            await self._repair_stale_busy_peers()
            await self._reap_dangling_peers()
            await self._evict_stale_peers()
            await self._emit_and_evict_expired_stashes()
            self._prune_delivery_traces()
            self._prune_spawn_ownership()
            self._prune_retired()
            self._save_events()
            self._persist_mappings()

    _RETIRED_TTL_SECONDS = 72 * 3600

    def _prune_retired(self) -> None:
        """Drop retirement records old enough that any orphan is long gone."""
        cutoff = datetime.now(timezone.utc) - timedelta(
            seconds=self._RETIRED_TTL_SECONDS
        )
        expired = [peer_id for peer_id, at in self._retired.items() if at <= cutoff]
        if not expired:
            return
        for peer_id in expired:
            self._retired.pop(peer_id, None)
        if self._state_db is not None:
            with self._state_db.conn:
                self._state_db.conn.executemany(
                    "DELETE FROM retired_peers WHERE peer_id = ?",
                    [(peer_id,) for peer_id in expired],
                )

    def _prune_spawn_ownership(self) -> None:
        """Drop spawn-ownership records pointing at dead tmux panes. Best-effort.

        After a daemon restart these rehydrate and otherwise cause kills to
        no-op (they point at a dead pane) and weaken kill/restart
        disambiguation. Piggy-backed on lazy_repair, never on a timer.
        """
        try:
            from repowire.spawn_ownership import prune_dead_ownership

            prune_dead_ownership()
        except Exception:  # noqa: BLE001 — pruning must not break repair
            logger.warning("Spawn-ownership prune failed", exc_info=True)

    def _prune_delivery_traces(self) -> None:
        """Drop delivery-trace rows older than prune_max_age_hours. Best-effort."""
        if self._state_db is None:
            return
        max_age_hours = self._daemon.prune_max_age_hours
        if not max_age_hours or max_age_hours <= 0:
            return
        try:
            cutoff = datetime.now(timezone.utc) - timedelta(hours=max_age_hours)
            DeliveryTraceStore(self._state_db).prune(cutoff.isoformat())
        except Exception:  # noqa: BLE001 - pruning must not break repair
            logger.warning("Delivery trace prune failed", exc_info=True)

    async def _demote_disconnected_peers(self) -> int:
        """Mark ONLINE/BUSY transport-owned peers without a WebSocket as OFFLINE.

        Catches ghost peers that registered via HTTP but whose ws-hook
        never connected (e.g. pane died before ws-hook could start).

        Pane-backed runtimes are preserved only when the daemon can still see
        runtime evidence (the recorded agent PID is alive or tmux still has
        the pane). A dropped ws-hook alone only means inbound transport is
        unavailable; absence of both socket and runtime evidence is a dead
        peer and should go OFFLINE.

        Peers carrying ``metadata["acp"]`` are exempt while the
        ``experiments.acp_broker_client`` flag is on: brokered ACP peers are
        live as long as their subprocess is, not their WebSocket. Demoting
        them solely because no WS exists would silently take them offline
        between asks and eventually reap them (repowire#206).
        """
        if not self._transport:
            return 0
        acp_flag = (
            bool(self._experiments.acp_broker_client)
            if self._experiments
            else False
        )
        async with self._lock:
            # Recovery: any peer with a live socket clears its connection
            # contradictions so a future recurrence re-emits exactly once.
            for p in self._peers.values():
                if self._transport.is_connected(p.peer_id):
                    self._clear_contradiction(p.peer_id, diag.ONLINE_BUT_NO_WS)
                    self._clear_contradiction(p.peer_id, diag.AGENT_PID_DEAD)
            candidates = [
                p for p in self._peers.values()
                if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and not self._transport.is_connected(p.peer_id)
                and not p.pane_id
                and not (acp_flag and p.metadata and p.metadata.get("acp"))
                # In-process service peers (@jobs) live exactly as long as the
                # daemon itself; they never own a WebSocket to lose.
                and not (p.metadata and p.metadata.get("in_process"))
            ]
            pane_candidates = [
                p for p in self._peers.values()
                if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and not self._transport.is_connected(p.peer_id)
                and p.pane_id
                and not (acp_flag and p.metadata and p.metadata.get("acp"))
            ]

        checks = await asyncio.gather(
            *(
                asyncio.to_thread(has_runtime_evidence, peer)
                for peer in pane_candidates
            ),
            return_exceptions=True,
        )
        pane_dead: set[str] = set()
        for peer, has_evidence in zip(pane_candidates, checks, strict=True):
            if has_evidence is True:
                continue
            candidates.append(peer)
            pane_dead.add(peer.peer_id)

        count = 0
        for peer in candidates:
            self._emit_contradiction(
                peer,
                diag.ONLINE_BUT_NO_WS,
                diag.SEVERITY_ERROR,
                f"peer is {peer.status.value} but has no live WebSocket connection",
            )
            if peer.peer_id in pane_dead and peer.agent_pid is not None:
                self._emit_contradiction(
                    peer,
                    diag.AGENT_PID_DEAD,
                    diag.SEVERITY_ERROR,
                    f"agent pid {peer.agent_pid} has no runtime evidence",
                )
            reason = (
                "no_websocket_no_runtime_evidence"
                if peer.peer_id in pane_dead
                else "no_websocket_no_pane"
            )
            await self.mark_offline(
                peer.peer_id,
                reason=reason,
                source="lazy_repair",
                detail=(
                    "Peer was ONLINE/BUSY without a live WebSocket and no "
                    "acceptable runtime evidence remained."
                ),
                context={
                    "pane_id": peer.pane_id,
                    "agent_pid": peer.agent_pid,
                    "contradiction": diag.ONLINE_BUT_NO_WS,
                },
            )
            count += 1
        if count:
            logger.info("demoted %d ghost peers (no WebSocket/runtime evidence)", count)
        return count

    async def _demote_unsafe_connected_peers(self) -> int:
        """Mark connected tmux peers OFFLINE if their pane is no longer safe."""
        transport = self._transport
        if not transport:
            return 0

        async with self._lock:
            targets = [
                p.peer_id for p in self._peers.values()
                if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
                and p.pane_id
                and p.backend != AgentType.OPENCODE
                and transport.is_connected(p.peer_id)
            ]

        async def check(peer_id: str) -> tuple[str, bool | None]:
            try:
                pong = await transport.ping(peer_id, timeout=1.0)
            except TimeoutError:
                return peer_id, None
            except asyncio.TimeoutError:
                return peer_id, None
            except Exception:
                return peer_id, None
            # Modern hooks omit pane_alive when the check was inconclusive
            # (tmux/ps shell-out failed); legacy hooks always send a bool.
            value = pong.get("pane_alive")
            return peer_id, (value if isinstance(value, bool) else None)

        results = await asyncio.gather(*(check(peer_id) for peer_id in targets))
        # Drop strike state for peers no longer tracked.
        self._pane_unsafe_strikes = {
            pid: n for pid, n in self._pane_unsafe_strikes.items()
            if pid in self._peers
        }
        count = 0
        for peer_id, pane_alive in results:
            if pane_alive is True:
                # Recovered (or never broken): allow a future PANE_MISSING to re-emit.
                self._pane_unsafe_strikes.pop(peer_id, None)
                self._clear_contradiction(peer_id, diag.PANE_MISSING)
                continue
            if pane_alive is None:
                # Inconclusive: neither a strike nor a recovery.
                continue
            strikes = self._pane_unsafe_strikes.get(peer_id, 0) + 1
            self._pane_unsafe_strikes[peer_id] = strikes
            if strikes < PANE_UNSAFE_STRIKE_LIMIT:
                continue
            self._pane_unsafe_strikes.pop(peer_id, None)
            peer = self._peers.get(peer_id)
            if peer is not None:
                self._emit_contradiction(
                    peer,
                    diag.PANE_MISSING,
                    diag.SEVERITY_ERROR,
                    f"connected pane {peer.pane_id} is no longer alive",
                )
            # Terminal: three honest "pane gone" verdicts retire the identity,
            # so the reporting ws-hook cannot reconnect it back to life.
            await self.mark_offline(
                peer_id,
                reason="pane_missing",
                source="lazy_repair",
                detail=(
                    "Connected ws-hook reported its tmux pane gone on "
                    f"{PANE_UNSAFE_STRIKE_LIMIT} consecutive pings."
                ),
                context={"contradiction": diag.PANE_MISSING},
                terminal=True,
            )
            count += 1

        if count:
            logger.info("demoted %d unsafe connected peers", count)
        return count

    async def _repair_stale_busy_peers(self) -> int:
        """Reset stale BUSY/working peers after missed terminal hooks.

        This is a demand-driven fallback for user interrupts/cancels where a
        backend does not emit Stop/AfterAgent. It intentionally ignores
        awaiting_input and any peer with recent liveness progress.
        """
        timeout = self._daemon.stale_busy_timeout_seconds
        if timeout <= 0:
            return 0

        cutoff = datetime.now(timezone.utc) - timedelta(seconds=timeout)
        async with self._lock:
            stale = [
                p for p in self._peers.values()
                if p.status == PeerStatus.BUSY
                and p.turn_state == "working"
                and p.last_seen is not None
                and p.last_seen < cutoff
            ]
            for peer in stale:
                old_status = peer.status
                peer.status = PeerStatus.ONLINE
                peer.turn_state = "idle"
                peer.last_seen = datetime.now(timezone.utc)
                self._emit_status_change(peer, old_status, PeerStatus.ONLINE)

        if stale:
            logger.info("repaired %d stale busy peers", len(stale))
        return len(stale)

    async def _reap_dangling_peers(self) -> int:
        """Remove OFFLINE peers whose liveness TTL has expired.

        Lazy-repair only: no background polling. ONLINE/BUSY peers are first
        demoted by the WebSocket liveness checks above; this pass removes peers
        that stayed offline past the configured grace window.
        """
        ttl = self._daemon.peer_reap_ttl_seconds
        if ttl <= 0:
            return 0

        cutoff = datetime.now(timezone.utc) - timedelta(seconds=ttl)
        async with self._lock:
            stale = [
                p for p in self._peers.values()
                if p.status == PeerStatus.OFFLINE
                and p.last_seen is not None
                and p.last_seen < cutoff
            ]
            for peer in stale:
                self._peers.pop(peer.peer_id, None)
                self._mappings.pop(peer.peer_id, None)
                self._description_set_at.pop(peer.peer_id, None)
                self._clear_all_contradictions(peer.peer_id)
                self._mappings_dirty = True

        # Snapshot doomed-with-stash asks BEFORE any destructive cleanup
        # so observers see the pending_reply_lost event before the ask
        # disappears. Order: snapshot → emit → forget/evict.
        doomed_stashes: list[tuple[Any, str]] = []
        if self._ask_tracker is not None:
            for peer in stale:
                snap = await self._ask_tracker.snapshot_pending_replies_for_peer(
                    peer.peer_id,
                )
                for ask in snap:
                    doomed_stashes.append((ask, "offline_ttl_reap"))

        for ask, reason in doomed_stashes:
            self._emit_pending_reply_lost(ask, reason)

        for peer in stale:
            if self._transport is not None:
                try:
                    await self._transport.disconnect(peer.peer_id)
                except Exception as e:
                    logger.warning(
                        "reaper: failed to disconnect %s transport: %s",
                        peer.peer_id,
                        e,
                    )
            if self._ask_tracker is not None:
                await self._ask_tracker.forget_peer(peer.peer_id)
            self.add_event(
                "peer_reaped",
                {
                    "peer_id": peer.peer_id,
                    "display_name": peer.display_name,
                    "backend": peer.backend.value,
                    "path": peer.path,
                    "pane_id": peer.pane_id,
                    "reason": "offline_ttl",
                },
            )

        if stale:
            logger.info("reaped %d dangling offline peers", len(stale))
        return len(stale)

    async def _evict_stale_peers(self) -> int:
        """Evict long-offline peers from both _peers and _mappings.

        Returns number of evicted peers.
        """
        max_age = self._daemon.prune_max_age_hours * 3600
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
                logger.info("evicted %d stale offline peers", len(stale))
        if stale and self._ask_tracker is not None:
            doomed_stashes: list[Any] = []
            for pid in stale:
                snap = await self._ask_tracker.snapshot_pending_replies_for_peer(pid)
                doomed_stashes.extend(snap)
            # snapshot → emit → forget so observers see the loss event
            # before the ask disappears.
            for ask in doomed_stashes:
                self._emit_pending_reply_lost(ask, "stale_evict")
            for pid in stale:
                await self._ask_tracker.forget_peer(pid)
        return len(stale)

    async def _emit_and_evict_expired_stashes(self) -> None:
        """Single owner for TTL-loss emission on stashed asks.

        Snapshot expired stashes, emit pointer-only ``pending_reply_lost``
        events, then call ``evict_expired(include_stashed=True)`` to delete.
        The Stop-hook-triggered ``_maybe_evict_expired`` path skips
        stashed asks specifically so this ordering can run.
        """
        if self._ask_tracker is None:
            return
        snap = await self._ask_tracker.snapshot_expired_pending_replies()
        # snapshot → emit → evict so observers see the loss event before
        # the ask disappears.
        for ask in snap:
            self._emit_pending_reply_lost(ask, "ttl_evicted")
        await self._ask_tracker.evict_expired(include_stashed=True)

    def _emit_pending_reply_lost(self, ask: Any, reason: str) -> None:
        """Pointer-only ``pending_reply_lost`` event.

        Carries enough to look up the lost correlation and the answerer
        that produced the reply; deliberately omits reply text, asker
        path, and asker machine.
        """
        ident = ask.asker_identity
        self.add_event(
            "pending_reply_lost",
            {
                "correlation_id": ask.correlation_id,
                "answerer_peer_id": ask.to_peer_id,
                "answerer_name": ask.to_peer_name,
                # asker_name is always present on the Ask itself, even when
                # asker_identity wasn't captured at stash time. Pointer-only:
                # never the reply text, never path, never machine.
                "asker_name": ask.from_peer_name,
                "asker_display_name": ident.display_name if ident else None,
                "asker_circle": ident.circle if ident else None,
                "asker_backend": ident.backend if ident else None,
                "asker_peer_id": ask.from_peer_id,
                "reason": reason,
                "pending_reply_at": (
                    ask.pending_reply_at.isoformat()
                    if ask.pending_reply_at else None
                ),
            },
        )

    async def active_repair(self) -> None:
        """Full liveness sweep: ping ONLINE/BUSY peers, mark dead ones OFFLINE.

        Unlike lazy_repair, this actively probes peers. Use for diagnostics
        or when lifecycle hooks are not available.
        """
        async with self._repair_lock:
            await self._do_repair()
            self._save_events()
            self._persist_mappings()

    async def _do_repair(self) -> None:
        """Ping/pong liveness check. Must hold _repair_lock."""
        transport = self._transport
        if not transport:
            return

        async with self._lock:
            targets = [
                p
                for p in self._peers.values()
                if p.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
            ]

        async def check_peer(peer: Peer) -> tuple[str, str | None] | None:
            """Returns (peer_id, circle) if alive, None if dead."""
            peer_id = peer.peer_id
            circle = peer.circle
            if not transport.is_connected(peer_id):
                has_evidence = await asyncio.to_thread(has_runtime_evidence, peer)
                return (peer_id, circle) if has_evidence else None
            if peer.backend == AgentType.OPENCODE:
                return (peer_id, circle)
            try:
                pong = await transport.ping(peer_id, timeout=5.0)
                pong_circle = pong.get("circle")
                return (peer_id, pong_circle or circle)
            except Exception:
                return None

        results = await asyncio.gather(
            *(check_peer(peer) for peer in targets),
            return_exceptions=True,
        )

        alive_peers = [r for r in results if isinstance(r, tuple)]
        dead_peer_ids = {p.peer_id for p in targets} - {r[0] for r in alive_peers}

        targets_map = {p.peer_id: p.circle for p in targets}
        for peer_id, new_circle in alive_peers:
            current = targets_map.get(peer_id)
            if current and new_circle and new_circle != current:
                logger.info(
                    "active_repair: circle recovery %s: %s → %s",
                    peer_id, current, new_circle,
                )
                await self.set_peer_circle(peer_id, new_circle)

        for peer_id in dead_peer_ids:
            logger.info("active_repair: marking %s OFFLINE (no pong)", peer_id)
            await self.mark_offline(
                peer_id,
                reason="active_repair_no_pong",
                source="active_repair",
                detail="Active repair could not prove the peer was alive.",
            )

        await self._evict_stale_peers()
