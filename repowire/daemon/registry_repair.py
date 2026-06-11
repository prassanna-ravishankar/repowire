"""Pure repair probes used by lazy peer-registry reconciliation."""

from __future__ import annotations

import os
import subprocess

from repowire.protocol.peers import Peer


def pid_alive(pid: int) -> bool:
    """True if `pid` exists (EPERM counts as alive — foreign owner)."""
    try:
        os.kill(pid, 0)
        return True
    except PermissionError:
        return True
    except OSError:
        return False


def tmux_pane_pid(pane_id: str) -> int | None:
    """Resolve a tmux pane's root process pid. None when unprobeable."""
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_pid}"],
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (FileNotFoundError, OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    try:
        return int(result.stdout.strip())
    except ValueError:
        return None


def process_ancestors(pid: int) -> set[int] | None:
    """Ancestor pids of ``pid`` (excluding itself). None when unprobeable.

    One ``ps`` snapshot, then a ppid walk (bounded against cycles). Used by
    the destructive pane-claim guard to tell a process that genuinely runs in
    a tmux pane from a subprocess that merely inherited TMUX_PANE.
    """
    try:
        result = subprocess.run(
            ["ps", "-axo", "pid=,ppid="],
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (FileNotFoundError, OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    parent: dict[int, int] = {}
    for line in result.stdout.splitlines():
        parts = line.split()
        if len(parts) != 2:
            continue
        try:
            parent[int(parts[0])] = int(parts[1])
        except ValueError:
            continue
    ancestors: set[int] = set()
    cur = pid
    while cur in parent and len(ancestors) < 128:
        cur = parent[cur]
        if cur in ancestors or cur <= 0:
            break
        ancestors.add(cur)
    return ancestors


def has_runtime_evidence(peer: Peer) -> bool:
    """Best-effort runtime proof for a disconnected pane-backed peer.

    This is demand-driven lazy repair, not polling. It intentionally checks only
    local process/tmux evidence and does not attempt any WebSocket recovery.
    """
    if peer.agent_pid is not None and peer.agent_pid > 0:
        # A recorded agent PID is the strongest runtime identity proof we
        # have. If it is gone, a leftover tmux pane/shell must not keep the
        # old peer alive.
        return pid_alive(peer.agent_pid)

    if not peer.pane_id:
        return False
    return tmux_pane_pid(peer.pane_id) is not None
