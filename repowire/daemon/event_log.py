"""Dashboard event log buffering and persistence."""

from __future__ import annotations

import asyncio
import json
import logging
from collections import deque
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from uuid import uuid4

from repowire.config.models import Config

logger = logging.getLogger(__name__)


class EventLog:
    """Bounded daemon event log with debounced disk persistence."""

    def __init__(self, path: Path | None = None, max_events: int = 500) -> None:
        self.path = path or (Config.get_config_dir() / "events.json")
        self.events: deque[dict[str, Any]] = deque(maxlen=max_events)
        self.dirty = False
        self.subscribers: set[asyncio.Event] = set()
        self.load()

    def load(self) -> None:
        """Load persisted events from disk."""
        try:
            if self.path.exists():
                data = json.loads(self.path.read_text())
                self.events.clear()
                for event in data[-100:]:
                    self.events.append(event)
        except Exception:
            logger.warning("Failed to load events from %s", self.path)

    def save(self) -> None:
        """Persist events to disk when dirty."""
        if not self.dirty:
            return
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self.path.write_text(json.dumps(list(self.events)))
            self.dirty = False
        except Exception:
            logger.warning("Failed to save events to %s", self.path)

    def add_event(self, event_type: str, data: dict[str, Any]) -> str:
        """Add an event to the history. Returns event ID."""
        event_id = str(uuid4())
        self.events.append(
            {
                "id": event_id,
                "type": event_type,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                **data,
            }
        )
        self.dirty = True
        for sub in self.subscribers:
            sub.set()
        return event_id

    def subscribe(self) -> asyncio.Event:
        """Register a wakeup Event fired on each add_event call."""
        evt = asyncio.Event()
        self.subscribers.add(evt)
        return evt

    def unsubscribe(self, evt: asyncio.Event) -> None:
        """Remove a subscriber Event registered via subscribe."""
        self.subscribers.discard(evt)

    def events_since(self, event_id: str | None) -> list[dict[str, Any]]:
        """Return events after the given id. If id is None or evicted, return all."""
        events = list(self.events)
        if event_id is None:
            return events
        for i, event in enumerate(events):
            if event["id"] == event_id:
                return events[i + 1 :]
        return events

    def update_event(self, event_id: str, updates: dict[str, Any]) -> bool:
        """Update an existing event by ID."""
        for event in self.events:
            if event["id"] == event_id:
                event.update(updates)
                self.dirty = True
                return True
        return False

    def get_events(self) -> list[dict[str, Any]]:
        """Get buffered events."""
        return list(self.events)
