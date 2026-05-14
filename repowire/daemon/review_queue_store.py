"""Persistent review queue: tracked PRs awaiting an agent's re-review.

The store records, per reviewer peer name, a list of PRs they have looked at
and the last SHA they reviewed. State enrichment (current HEAD SHA, PR state)
happens at read time via `gh api`; the store itself is dumb durable state.

Lazy-repair compatible: writes happen synchronously inside route handlers
(small file, low frequency, no polling needed). The atomic-rename pattern
mirrors `peer_registry._persist_mappings`.
"""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path

logger = logging.getLogger(__name__)


@dataclass
class ReviewEntry:
    """A tracked PR for a single reviewer."""

    pr_url: str
    last_reviewed_sha: str | None = None
    recorded_at: str = field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )


class ReviewQueueStore:
    """JSON-backed store of reviewer -> [ReviewEntry].

    All public methods are thread-safe; the underlying file is rewritten
    atomically on every mutation. The data set is small (one row per tracked
    PR per reviewer) so we don't bother with a dirty-flag debounce.
    """

    def __init__(self, path: Path) -> None:
        self._path = path
        self._lock = threading.Lock()
        self._data: dict[str, list[ReviewEntry]] = {}
        self._load()

    @property
    def path(self) -> Path:
        return self._path

    def _load(self) -> None:
        if not self._path.exists():
            return
        try:
            raw = json.loads(self._path.read_text())
        except (json.JSONDecodeError, OSError, ValueError) as e:
            logger.error("Corrupt review queue at %s: %s", self._path, e)
            return
        if not isinstance(raw, dict):
            logger.error("Review queue file has wrong shape (expected dict)")
            return
        for reviewer, entries in raw.items():
            if not isinstance(entries, list):
                continue
            parsed: list[ReviewEntry] = []
            for item in entries:
                if not isinstance(item, dict) or "pr_url" not in item:
                    continue
                parsed.append(
                    ReviewEntry(
                        pr_url=item["pr_url"],
                        last_reviewed_sha=item.get("last_reviewed_sha"),
                        recorded_at=item.get(
                            "recorded_at",
                            datetime.now(timezone.utc).isoformat(),
                        ),
                    )
                )
            self._data[reviewer] = parsed

    def _persist(self) -> None:
        tmp_path = self._path.with_suffix(self._path.suffix + ".tmp")
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            payload = {
                reviewer: [asdict(e) for e in entries]
                for reviewer, entries in self._data.items()
            }
            tmp_path.write_text(json.dumps(payload, indent=2))
            os.replace(str(tmp_path), str(self._path))
        except OSError as e:
            logger.error("Failed to save review queue: %s", e)
            try:
                tmp_path.unlink(missing_ok=True)
            except OSError:
                pass

    def upsert(
        self, reviewer: str, pr_url: str, last_reviewed_sha: str | None
    ) -> ReviewEntry:
        """Insert or update an entry. Returns the stored row."""
        with self._lock:
            entries = self._data.setdefault(reviewer, [])
            now = datetime.now(timezone.utc).isoformat()
            for entry in entries:
                if entry.pr_url == pr_url:
                    entry.last_reviewed_sha = last_reviewed_sha
                    entry.recorded_at = now
                    self._persist()
                    return entry
            entry = ReviewEntry(
                pr_url=pr_url,
                last_reviewed_sha=last_reviewed_sha,
                recorded_at=now,
            )
            entries.append(entry)
            self._persist()
            return entry

    def list_for(self, reviewer: str) -> list[ReviewEntry]:
        with self._lock:
            return list(self._data.get(reviewer, []))

    def delete(self, reviewer: str, pr_url: str) -> bool:
        """Remove an entry. Returns True if something was removed."""
        with self._lock:
            entries = self._data.get(reviewer)
            if not entries:
                return False
            new_entries = [e for e in entries if e.pr_url != pr_url]
            if len(new_entries) == len(entries):
                return False
            if new_entries:
                self._data[reviewer] = new_entries
            else:
                del self._data[reviewer]
            self._persist()
            return True
