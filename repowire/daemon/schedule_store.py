"""Scheduled check-in persistence.

A scheduled check-in is a one-shot future delivery: at `fire_at`, the daemon
injects a `notify` or `ask` to `to_peer` on behalf of `from_peer`. Recurring
schedules are intentionally out of scope for the MVP.

Persistence mirrors PeerRegistry's pattern: in-memory dict + dirty flag +
debounced atomic-rename writes triggered at mutation time and on shutdown.
"""

from __future__ import annotations

import json
import logging
import os
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Protocol
from uuid import uuid4

from repowire.daemon.schedule_cron import next_fire_after, validate_cron

logger = logging.getLogger(__name__)

ScheduleKind = str  # "ask" | "notify"
_VALID_KINDS = frozenset({"ask", "notify"})


@dataclass
class Schedule:
    """A single one-shot scheduled delivery."""

    schedule_id: str
    from_peer: str
    to_peer: str
    text: str
    fire_at: str  # ISO-8601 UTC
    kind: ScheduleKind = "notify"
    circle: str | None = None
    cron: str | None = None
    created_at: str = field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat(),
    )

    def fire_at_dt(self) -> datetime:
        return datetime.fromisoformat(self.fire_at)


class ScheduleStoreProtocol(Protocol):
    """Persistence adapter interface used by the scheduler and routes."""

    def persist(self) -> None: ...

    def create(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        fire_at: datetime,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        cron: str | None = None,
    ) -> Schedule: ...

    def create_cron(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        cron: str,
        *,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        now: datetime | None = None,
    ) -> Schedule: ...

    def reschedule_next(self, schedule_id: str, after: datetime | None = None) -> bool: ...

    def delete(self, schedule_id: str) -> Schedule | None: ...

    def get(self, schedule_id: str) -> Schedule | None: ...

    def list_all(self, from_peer: str | None = None) -> list[Schedule]: ...

    def next_due(self) -> Schedule | None: ...


class ScheduleStore:
    """In-memory schedule registry with disk persistence.

    Not thread-safe across coroutines that need consistency; the caller
    (Scheduler) holds the lock that matters. Disk writes are debounced via a
    dirty flag.
    """

    def __init__(self, persistence_path: Path) -> None:
        self._path = persistence_path
        self._schedules: dict[str, Schedule] = {}
        self._dirty = False
        self._load()

    def _load(self) -> None:
        if not self._path.exists():
            return
        try:
            data = json.loads(self._path.read_text())
            for sid, raw in data.items():
                self._schedules[sid] = Schedule(**raw)
            logger.info("Loaded %d scheduled check-ins", len(self._schedules))
        except (json.JSONDecodeError, TypeError, ValueError, KeyError) as e:
            logger.error("Corrupt schedules file (%s); starting empty", e)
            self._schedules = {}
            self._dirty = True
        except OSError as e:
            logger.error("Failed to read schedules: %s", e)

    def persist(self) -> None:
        """Flush to disk if dirty."""
        if not self._dirty:
            return
        tmp = self._path.with_suffix(".json.tmp")
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            data = {sid: asdict(s) for sid, s in self._schedules.items()}
            tmp.write_text(json.dumps(data, indent=2))
            os.replace(str(tmp), str(self._path))
            self._dirty = False
        except OSError as e:
            logger.error("Failed to save schedules: %s", e)
            try:
                tmp.unlink(missing_ok=True)
            except OSError:
                pass

    def create(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        fire_at: datetime,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        cron: str | None = None,
    ) -> Schedule:
        if kind not in _VALID_KINDS:
            raise ValueError(f"kind must be one of {sorted(_VALID_KINDS)}; got {kind!r}")
        if fire_at.tzinfo is None:
            raise ValueError("fire_at must be timezone-aware")
        normalized_cron = validate_cron(cron) if cron is not None else None
        sid = f"sched-{uuid4().hex[:8]}"
        sched = Schedule(
            schedule_id=sid,
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            fire_at=fire_at.astimezone(timezone.utc).isoformat(),
            kind=kind,
            circle=circle,
            cron=normalized_cron,
        )
        self._schedules[sid] = sched
        self._dirty = True
        return sched

    def create_cron(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        cron: str,
        *,
        kind: ScheduleKind = "notify",
        circle: str | None = None,
        now: datetime | None = None,
    ) -> Schedule:
        """Create a recurring schedule from a cron expression."""
        base = now or datetime.now(timezone.utc)
        normalized_cron = validate_cron(cron)
        return self.create(
            from_peer=from_peer,
            to_peer=to_peer,
            text=text,
            fire_at=next_fire_after(normalized_cron, base),
            kind=kind,
            circle=circle,
            cron=normalized_cron,
        )

    def reschedule_next(self, schedule_id: str, after: datetime | None = None) -> bool:
        """Advance a recurring schedule to its next fire time."""
        sched = self._schedules.get(schedule_id)
        if sched is None or sched.cron is None:
            return False
        base = after or datetime.now(timezone.utc)
        sched.fire_at = next_fire_after(sched.cron, base).isoformat()
        self._dirty = True
        return True

    def delete(self, schedule_id: str) -> Schedule | None:
        s = self._schedules.pop(schedule_id, None)
        if s is not None:
            self._dirty = True
        return s

    def get(self, schedule_id: str) -> Schedule | None:
        return self._schedules.get(schedule_id)

    def list_all(self, from_peer: str | None = None) -> list[Schedule]:
        items = list(self._schedules.values())
        if from_peer is not None:
            items = [s for s in items if s.from_peer == from_peer]
        items.sort(key=lambda s: s.fire_at)
        return items

    def next_due(self) -> Schedule | None:
        """Return the schedule with the earliest fire_at, or None if empty."""
        if not self._schedules:
            return None
        return min(self._schedules.values(), key=lambda s: s.fire_at)
