"""Spawn hint files: bridge spawn intent to runtimes that strip tmux env.

When the daemon spawns a peer via /spawn, the requested circle is encoded only
as the tmux session name. Most runtimes (Claude Code, Gemini) inherit TMUX env
into MCP/hook subprocesses, so they can read the session name back from
`tmux display-message`. Codex sandboxes its MCP and hook subprocesses with a
minimal env (no TMUX, TMUX_PANE, PWD), so it cannot recover the session name —
the eager `_ensure_registered` and the SessionStart hook both fall back to
circle="default".

To bridge that gap, the spawn flow drops a small JSON hint file under
`~/.cache/repowire/spawn-hints/` keyed by (path, backend). Registration
fallbacks consult this hint before falling back to "default" and consume one
queued entry on use. Hints have a short TTL so stale ones never override later
spawns.
"""

from __future__ import annotations

import hashlib
import json
import logging
import time
from contextlib import suppress
from pathlib import Path

from repowire.config.models import CACHE_DIR

logger = logging.getLogger(__name__)

# Hints older than this are ignored and treated as garbage. Spawn → MCP boot
# → eager register usually completes within a few seconds; 5 minutes is
# generous slack for slow hosts and codex's late-fired SessionStart.
HINT_TTL_SECONDS = 300

HintPayload = dict[str, object]


def _hints_dir() -> Path:
    path = CACHE_DIR / "spawn-hints"
    path.mkdir(parents=True, exist_ok=True)
    return path


def _hint_key(path: str, backend: str) -> str:
    raw = f"{Path(path).resolve()}::{backend}".encode()
    return hashlib.sha256(raw).hexdigest()[:16]


def _hint_path(path: str, backend: str) -> Path:
    return _hints_dir() / f"{_hint_key(path, backend)}.json"


def write_hint(
    path: str,
    backend: str,
    circle: str,
    *,
    role: str | None = None,
    peer_id: str | None = None,
    pending_first_turn: bool = False,
) -> None:
    """Record spawn intent so a peer registering from this path+backend can
    discover its requested circle (and optionally role).

    ``pending_first_turn=True`` marks spawned peers that have a seed message
    in flight but no first turn yet -- the session handler uses it to set
    the peer's initial turn_state, so orchestrators can spot
    spawn-seed-drops and re-send the brief.
    """
    payload: HintPayload = {
        "path": str(Path(path).resolve()),
        "backend": backend,
        "circle": circle,
        "ts": time.time(),
    }
    if role:
        payload["role"] = role
    if peer_id:
        payload["peer_id"] = peer_id
    if pending_first_turn:
        payload["pending_first_turn"] = True
    target = _hint_path(path, backend)
    try:
        existing = _read_hint_queue(target)
        existing.append(payload)
        target.write_text(json.dumps(existing))
    except OSError as e:
        logger.warning("spawn_hints: failed to write %s: %s", target, e)


def consume_hint(path: str, backend: str) -> str | None:
    """Read and delete the spawn hint for (path, backend).

    Returns the requested circle, or None if no fresh hint exists. Stale
    hints (older than HINT_TTL_SECONDS) are deleted and treated as missing.

    Use consume_hint_full() if the role hint is also needed; this thin
    wrapper preserves the legacy str-returning contract.
    """
    data = consume_hint_full(path, backend)
    return data["circle"] if data else None


def consume_hint_full(path: str, backend: str) -> dict | None:
    """Read and delete the spawn hint for (path, backend), returning the full payload.

    Returns a dict with at least 'circle' (str), and optionally 'role' (str)
    and 'peer_id' (str). None when no fresh hint exists.
    """
    target = _hint_path(path, backend)
    queue = _read_hint_queue(target)
    if not queue:
        with suppress(OSError):
            target.unlink()
        return None

    now = time.time()
    fresh: list[HintPayload] = []
    selected: HintPayload | None = None
    for item in queue:
        ts = item.get("ts")
        circle = item.get("circle")
        if not isinstance(ts, (int, float)) or not isinstance(circle, str):
            continue
        if now - ts > HINT_TTL_SECONDS:
            continue
        if selected is None:
            selected = item
        else:
            fresh.append(item)

    if fresh:
        with suppress(OSError):
            target.write_text(json.dumps(fresh))
    else:
        with suppress(OSError):
            target.unlink()

    if selected is None:
        return None

    out: dict = {"circle": selected["circle"]}
    role = selected.get("role")
    if isinstance(role, str) and role:
        out["role"] = role
    peer_id = selected.get("peer_id")
    if isinstance(peer_id, str) and peer_id:
        out["peer_id"] = peer_id
    if selected.get("pending_first_turn") is True:
        out["pending_first_turn"] = True
    return out


def _read_hint_queue(target: Path) -> list[HintPayload]:
    """Return queued hint payloads, accepting the legacy single-dict format."""
    try:
        raw = target.read_text()
    except OSError:
        return []

    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        with suppress(OSError):
            target.unlink()
        return []

    if isinstance(data, dict):
        return [data]
    if isinstance(data, list):
        return [item for item in data if isinstance(item, dict)]
    with suppress(OSError):
        target.unlink()
    return []
