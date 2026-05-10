"""Shared tmux utilities for hooks."""

from __future__ import annotations

import logging
import os
import shutil
import subprocess
from typing import TypedDict

logger = logging.getLogger(__name__)


class TmuxInfo(TypedDict):
    """Tmux environment information.

    The pane_id is the raw tmux pane ID (e.g., "%42"). It is used as a
    filename stem for .sid, .pid, correlation, and response cache files.
    The canonical peer_id is assigned by SessionMapper at WebSocket connect.
    """

    pane_id: str | None  # tmux pane ID, used as filename stem for hook files
    session_name: str | None
    window_name: str | None


def is_tmux_available() -> bool:
    """Check if tmux is installed and a server is reachable."""
    if not shutil.which("tmux"):
        return False
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-p", ""],
            capture_output=True,
            timeout=3,
        )
        return result.returncode == 0
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        return False


def _get_ppid_chain(max_depth: int = 16) -> list[int]:
    """Walk getppid() ancestry. Returns pids from immediate parent upward."""
    chain: list[int] = []
    try:
        pid = os.getppid()
    except OSError:
        return chain
    seen: set[int] = set()
    for _ in range(max_depth):
        if pid <= 1 or pid in seen:
            break
        seen.add(pid)
        chain.append(pid)
        try:
            result = subprocess.run(
                ["ps", "-o", "ppid=", "-p", str(pid)],
                capture_output=True,
                text=True,
                timeout=2,
            )
        except (subprocess.SubprocessError, FileNotFoundError, OSError):
            break
        if result.returncode != 0:
            break
        try:
            pid = int(result.stdout.strip())
        except ValueError:
            break
    return chain


def _resolve_pane_via_ppid_chain() -> str | None:
    """Match an ancestor pid against tmux pane_pids to identify the spawning pane.

    Returns the pane_id whose shell pid appears in our process ancestry, or
    None if no match. Handles the multi-pane case where TMUX_PANE wasn't
    inherited: `tmux display-message` would return the focused pane (often
    a different peer's), but ancestor matching is unambiguous.

    Caveat: if Claude Code's MCP subprocess re-parents to init/launchd
    (daemonizes), the ancestor chain detaches from the pane shell and this
    returns None — the caller falls through to `tmux display-message`, which
    in multi-peer-same-cwd scenarios will mislabel the peer. There is no
    known reliable workaround for the re-parented case; in practice Claude
    Code keeps MCP subprocesses attached.
    """
    try:
        result = subprocess.run(
            ["tmux", "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}"],
            capture_output=True,
            text=True,
            timeout=3,
        )
    except (subprocess.SubprocessError, FileNotFoundError, OSError) as e:
        logger.debug("ppid-chain pane resolution: tmux list-panes failed: %s", e)
        return None
    if result.returncode != 0:
        logger.debug(
            "ppid-chain pane resolution: tmux list-panes rc=%d", result.returncode
        )
        return None

    pane_by_pid: dict[int, str] = {}
    for line in result.stdout.splitlines():
        parts = line.strip().split()
        if len(parts) != 2:
            continue
        try:
            pane_by_pid[int(parts[1])] = parts[0]
        except ValueError:
            continue

    for pid in _get_ppid_chain():
        if pid in pane_by_pid:
            return pane_by_pid[pid]

    logger.debug("ppid-chain pane resolution: no ancestor pid matched any tmux pane")
    return None


def get_pane_id() -> str | None:
    """Get the current tmux pane ID.

    Resolution order:
    1. TMUX_PANE env var (cheapest, authoritative when inherited)
    2. ppid-chain match against `tmux list-panes` pane_pids (handles
       MCP subprocesses that don't inherit TMUX_PANE — unambiguous even
       when multiple panes are alive)
    3. `tmux display-message` (last resort; returns focused pane, which
       is wrong in multi-peer-same-cwd scenarios — see #107)
    """
    pane_id = os.environ.get("TMUX_PANE")
    if pane_id:
        return pane_id

    # Only attempt tmux queries if TMUX env var is set (proves we're inside a
    # tmux session). Without this guard, we'd get a pane from an unrelated
    # session.
    if not os.environ.get("TMUX"):
        return None

    pane_id = _resolve_pane_via_ppid_chain()
    if pane_id:
        return pane_id

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-p", "#{pane_id}"],
            capture_output=True,
            text=True,
            timeout=3,
        )
        if result.returncode == 0:
            pane_id = result.stdout.strip()
            if pane_id:
                return pane_id
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        pass

    return None


def get_tmux_info() -> TmuxInfo:
    """Get full tmux environment info.

    Returns a dict with pane_id, session_name, and window_name.
    All values will be None if not running in tmux.
    """
    pane_id = get_pane_id()
    if not pane_id:
        return {"pane_id": None, "session_name": None, "window_name": None}

    session_name = None
    window_name = None

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{session_name}:#{window_name}"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            parts = result.stdout.strip().split(":", 1)
            if len(parts) == 2:
                session_name, window_name = parts
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        pass

    return {"pane_id": pane_id, "session_name": session_name, "window_name": window_name}
