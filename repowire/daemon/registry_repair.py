"""Pure repair probes used by lazy peer-registry reconciliation."""

from __future__ import annotations

import os
import subprocess

from repowire.protocol.peers import Peer


def has_runtime_evidence(peer: Peer) -> bool:
    """Best-effort runtime proof for a disconnected pane-backed peer.

    This is demand-driven lazy repair, not polling. It intentionally checks only
    local process/tmux evidence and does not attempt any WebSocket recovery.
    """
    if peer.agent_pid is not None and peer.agent_pid > 0:
        try:
            os.kill(peer.agent_pid, 0)
            return True
        except PermissionError:
            return True
        except OSError:
            # A recorded agent PID is the strongest runtime identity proof we
            # have. If it is gone, a leftover tmux pane/shell must not keep the
            # old peer alive.
            return False

    if not peer.pane_id:
        return False
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", peer.pane_id, "-p", "#{pane_pid}"],
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (FileNotFoundError, OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0 and bool(result.stdout.strip())
