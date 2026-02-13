"""Session mapping for stable peer IDs.

Maps stable session_id (repow-<circle>-<uuid8>) to connection state.
Survives tmux pane movements and WebSocket reconnects.
"""

import json
import logging
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from uuid import uuid4

from repowire.config.models import AgentType

logger = logging.getLogger(__name__)


@dataclass
class SessionMapping:
    """Persistent mapping of session to peer identity."""

    session_id: str  # "repow-dev-a1b2c3d4"
    display_name: str
    circle: str
    backend: AgentType
    path: str | None = None
    updated_at: str | None = None

    def __post_init__(self) -> None:
        if self.updated_at is None:
            self.updated_at = datetime.now(timezone.utc).isoformat()


class SessionMapper:
    """Maps stable session_id to connection state."""

    def __init__(self, persistence_path: Path):
        self._path = persistence_path
        self._mappings: dict[str, SessionMapping] = {}
        self._load()

    def _load(self) -> None:
        """Load mappings from disk."""
        if not self._path.exists():
            return

        try:
            data = json.loads(self._path.read_text())
            for session_id, mapping_data in data.items():
                self._mappings[session_id] = SessionMapping(**mapping_data)
            logger.info(f"Loaded {len(self._mappings)} session mappings")
        except Exception as e:
            logger.error(f"Failed to load session mappings: {e}")

    def _save(self) -> None:
        """Save mappings to disk."""
        self._path.parent.mkdir(parents=True, exist_ok=True)
        data = {session_id: asdict(mapping) for session_id, mapping in self._mappings.items()}
        self._path.write_text(json.dumps(data, indent=2))

    def register_session(
        self,
        display_name: str,
        circle: str,
        backend: AgentType,
        path: str | None = None,
    ) -> str:
        """Register or reuse session_id for a peer.

        If a session with the same display_name and circle exists,
        reuse its session_id. Otherwise, generate a new one.

        Returns:
            session_id (e.g., "repow-dev-a1b2c3d4")
        """
        # Check for existing session with same name+circle
        for sid, mapping in self._mappings.items():
            if mapping.display_name == display_name and mapping.circle == circle:
                # Reuse existing session_id
                mapping.backend = backend  # Update backend if changed
                mapping.path = path
                mapping.updated_at = datetime.now(timezone.utc).isoformat()
                self._save()
                logger.info(f"Reusing session {sid} for {display_name}@{circle}")
                return sid

        # Generate new session_id
        session_id = f"repow-{circle}-{uuid4().hex[:8]}"
        self._mappings[session_id] = SessionMapping(
            session_id=session_id,
            display_name=display_name,
            circle=circle,
            backend=backend,
            path=path,
        )
        self._save()
        logger.info(f"Created session {session_id} for {display_name}@{circle}")
        return session_id

    def get_mapping(self, session_id: str) -> SessionMapping | None:
        """Get mapping for session_id."""
        return self._mappings.get(session_id)

    def get_all_mappings(self) -> dict[str, SessionMapping]:
        """Get all mappings."""
        return self._mappings.copy()

    def unregister_session(self, session_id: str) -> bool:
        """Unregister session (remove from persistence)."""
        if session_id in self._mappings:
            del self._mappings[session_id]
            self._save()
            logger.info(f"Unregistered session {session_id}")
            return True
        return False
