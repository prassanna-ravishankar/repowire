"""Read-only Beads board snapshot for the dashboard.

Reuses Beads + Dolt as the work ledger rather than inventing a parallel store:
the daemon shells ``bd list --json`` (the stable JSON contract) and returns a
slim, bounded snapshot grouped by lifecycle state. Read-only — only ``bd
ready`` / ``bd list`` (never a write or ``dolt`` sync verb) are invoked. If
``bd`` is missing, the repo has no Beads workspace, the command times out, or
the JSON is malformed, the snapshot reports ``available=False`` instead of
raising, so the panel degrades cleanly.

Scope: the board reads Beads for the daemon's own repo (the project this
dashboard serves), located by walking up for a ``.beads`` directory. Multi-
project boards would need an explicit project selector (not this slice).
"""

from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path
from typing import Any, TypedDict

# Per-group caps so a large backlog can't bloat the dashboard payload.
_GROUP_LIMIT = 50
_RECENTLY_CLOSED_LIMIT = 15
_BD_TIMEOUT_SECONDS = 5.0


class BeadsRow(TypedDict):
    id: str
    title: str
    status: str
    priority: int | None
    issue_type: str | None
    assignee: str | None


class BeadsGroup(TypedDict):
    items: list[BeadsRow]
    total: int  # full count before the per-group cap
    truncated: bool


class BeadsBoard(TypedDict):
    available: bool
    ready: BeadsGroup
    in_progress: BeadsGroup
    blocked: BeadsGroup
    recently_closed: BeadsGroup


def _empty_group() -> BeadsGroup:
    return BeadsGroup(items=[], total=0, truncated=False)


def _empty_board(available: bool) -> BeadsBoard:
    return BeadsBoard(
        available=available,
        ready=_empty_group(),
        in_progress=_empty_group(),
        blocked=_empty_group(),
        recently_closed=_empty_group(),
    )


def _find_beads_root(start: Path | None = None) -> Path | None:
    """Walk up from ``start`` (default cwd) for a directory containing ``.beads``."""
    here = (start or Path.cwd()).resolve()
    for d in (here, *here.parents):
        if (d / ".beads").is_dir():
            return d
    return None


def _run_bd(args: list[str], cwd: Path) -> list[dict[str, Any]] | None:
    """Run a read-only ``bd`` command returning JSON, or None on any failure.

    Only ``ready`` / ``list`` are ever passed here — never a write or sync verb.
    """
    try:
        result = subprocess.run(
            ["bd", *args, "--json"],
            capture_output=True,
            text=True,
            timeout=_BD_TIMEOUT_SECONDS,
            cwd=str(cwd),
        )
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        return None
    if result.returncode != 0:
        return None
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return None
    return data if isinstance(data, list) else None


def _slim(row: dict[str, Any]) -> BeadsRow:
    return BeadsRow(
        id=str(row.get("id", "")),
        title=str(row.get("title", "")),
        status=str(row.get("status", "")),
        priority=row.get("priority"),
        issue_type=row.get("issue_type"),
        assignee=row.get("assignee") or None,
    )


def _group(rows: list[dict[str, Any]], limit: int) -> BeadsGroup:
    total = len(rows)
    return BeadsGroup(
        items=[_slim(r) for r in rows[:limit]],
        total=total,
        truncated=total > limit,
    )


def read_beads_board() -> BeadsBoard:
    """Snapshot of ready / in-progress / blocked / recently-closed Beads work.

    ``ready`` uses ``bd ready`` (respects blockers); the others use
    ``bd list --status=...``. Every group is bounded. Returns
    ``available=False`` when ``bd`` is absent, no Beads workspace is found, or
    every lookup fails — callers render a degraded panel rather than erroring.
    """
    if shutil.which("bd") is None:
        return _empty_board(available=False)
    root = _find_beads_root()
    if root is None:
        return _empty_board(available=False)

    ready = _run_bd(["ready"], root)
    in_progress = _run_bd(["list", "--status=in_progress"], root)
    blocked = _run_bd(["list", "--status=blocked"], root)
    closed = _run_bd(["list", "--status=closed"], root)
    if ready is None and in_progress is None and blocked is None and closed is None:
        return _empty_board(available=False)

    closed_sorted = sorted(
        closed or [], key=lambda r: str(r.get("updated_at", "")), reverse=True
    )

    return BeadsBoard(
        available=True,
        ready=_group(ready or [], _GROUP_LIMIT),
        in_progress=_group(in_progress or [], _GROUP_LIMIT),
        blocked=_group(blocked or [], _GROUP_LIMIT),
        recently_closed=_group(closed_sorted, _RECENTLY_CLOSED_LIMIT),
    )
