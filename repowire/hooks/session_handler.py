#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""

from __future__ import annotations

import json
import os
import signal
import socket
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

from repowire.hooks._tmux import get_tmux_info

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def get_peer_name(cwd: str) -> str:
    """Generate a peer name from the working directory (folder name)."""
    return Path(cwd).name


def get_git_branch(cwd: str) -> str | None:
    """Get current git branch for the working directory."""
    try:
        result = subprocess.run(
            ["git", "branch", "--show-current"],
            capture_output=True,
            text=True,
            cwd=cwd,
        )
        if result.returncode == 0:
            branch = result.stdout.strip()
            return branch if branch else None
    except Exception:
        pass
    return None


def fetch_peers() -> list[dict] | None:
    """Fetch current peers from the daemon."""
    try:
        req = urllib.request.Request(f"{DAEMON_URL}/peers", method="GET")
        with urllib.request.urlopen(req, timeout=2.0) as resp:
            if resp.status == 200:
                data = json.loads(resp.read().decode())
                return data.get("peers", [])
    except json.JSONDecodeError as e:
        print(f"repowire session: invalid JSON from daemon /peers: {e}", file=sys.stderr)
    except (urllib.error.URLError, urllib.error.HTTPError, OSError):
        pass
    return None


def format_peers_context(peers: list[dict], my_name: str) -> str:
    """Format peers into context string for Claude."""
    other_peers = [p for p in peers if p["name"] != my_name and p["status"] == "online"]

    if not other_peers:
        return ""

    lines = [
        "[Repowire Mesh] You have access to other Claude Code sessions working on related projects:"
    ]
    for p in other_peers:
        branch = p.get("metadata", {}).get("branch", "")
        branch_str = f" on {branch}" if branch else ""
        project_name = Path(p.get("path", "")).name or p["name"]
        lines.append(f"  - {p['name']}{branch_str} ({project_name})")

    lines.append("")
    lines.append(
        "IMPORTANT: When asked about these projects, ask the peer directly "
        "via ask_peer() rather than searching locally."
    )
    lines.append("Peer list may be outdated - use list_peers() to refresh.")

    return "\n".join(lines)


def get_machine_name() -> str:
    """Get the machine hostname."""
    return socket.gethostname()


def register_peer(
    peer_id: str,
    display_name: str,
    cwd: str,
    machine: str,
    tmux_target: str | None,
    session_id: str,
    metadata: dict,
) -> bool:
    """Register peer with daemon via HTTP.

    Args:
        peer_id: Unique peer ID (tmux pane ID like "%42" for Claude Code)
        display_name: Human-readable name (folder name)
        cwd: Working directory path
        machine: Machine hostname
        tmux_target: Tmux session:window target
        session_id: Claude session ID
        metadata: Additional metadata (e.g., git branch)

    Returns:
        True if registration succeeded, False otherwise.
    """
    try:
        data = {
            "peer_id": peer_id,
            "pane_id": peer_id,  # Backward compat
            "display_name": display_name,
            "name": display_name,  # Backward compat
            "path": cwd,
            "machine": machine,
            "tmux_session": tmux_target,
            "session_id": session_id,
            "metadata": metadata,
        }
        req = urllib.request.Request(
            f"{DAEMON_URL}/peers",
            data=json.dumps(data).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        urllib.request.urlopen(req, timeout=2.0)
        return True
    except (urllib.error.URLError, urllib.error.HTTPError, OSError):
        return False


def main() -> int:
    """Main entry point."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire session: invalid JSON input: {e}", file=sys.stderr)
        return 0

    event = input_data.get("hook_event_name")
    cwd = input_data.get("cwd", os.getcwd())

    # Get tmux info (pane_id used for tmux targeting)
    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]

    # display_name is the folder name (human-readable)
    display_name = get_peer_name(cwd)

    if event == "SessionStart":
        # Launch async WebSocket hook in background
        # This maintains persistent WebSocket connection for queries/notifications
        try:
            # Find the websocket_hook.py script
            hook_script = Path(__file__).parent / "websocket_hook.py"
            if hook_script.exists():
                # Start async hook as background process
                # Pass cwd so the hook registers with the correct project name
                log_dir = Path.home() / ".cache" / "repowire" / "logs"
                log_dir.mkdir(parents=True, exist_ok=True)
                pane_log = (pane_id or "unknown").replace("%", "")
                log_file = open(log_dir / f"ws-hook-{pane_log}.log", "w")  # noqa: SIM115
                proc = subprocess.Popen(
                    [sys.executable, str(hook_script)],
                    stdout=log_file,
                    stderr=log_file,
                    start_new_session=True,
                    cwd=cwd,
                )
                log_file.close()  # subprocess inherits the fd
                # Store PID for cleanup on SessionEnd
                pid_dir = Path.home() / ".cache" / "repowire" / "hooks"
                pid_dir.mkdir(parents=True, exist_ok=True)
                pane_file = (pane_id or "unknown").replace("%", "")
                (pid_dir / f"{pane_file}.pid").write_text(str(proc.pid))
        except Exception as e:
            print(f"repowire: failed to start WebSocket hook: {e}", file=sys.stderr)

        # Fetch peers and output context for Claude
        # Note: Registration is now handled by the WebSocket hook's connect message
        peers = fetch_peers()
        if peers:
            context = format_peers_context(peers, display_name)
            if context:
                output = {
                    "hookSpecificOutput": {
                        "hookEventName": "SessionStart",
                        "additionalContext": context,
                    }
                }
                print(json.dumps(output))

    elif event == "SessionEnd":
        # Kill the WebSocket hook process
        if pane_id:
            pane_file = pane_id.replace("%", "")
            pid_file = Path.home() / ".cache" / "repowire" / "hooks" / f"{pane_file}.pid"
            if pid_file.exists():
                try:
                    pid = int(pid_file.read_text().strip())
                    os.kill(pid, signal.SIGTERM)
                except (ValueError, OSError, ProcessLookupError):
                    pass
                finally:
                    pid_file.unlink(missing_ok=True)

        # Mark peer offline so pending queries get cancelled
        # Prefer session_id (unambiguous) over display_name
        peer_identifier = display_name
        if pane_id:
            pane_clean = pane_id.replace("%", "")
            sid_file = Path.home() / ".cache" / "repowire" / "hooks" / f"{pane_clean}.sid"
            if sid_file.exists():
                try:
                    peer_identifier = sid_file.read_text().strip()
                except OSError:
                    pass
                finally:
                    sid_file.unlink(missing_ok=True)

        try:
            req = urllib.request.Request(
                f"{DAEMON_URL}/peers/{peer_identifier}/offline",
                method="POST",
            )
            urllib.request.urlopen(req, timeout=2.0)
        except urllib.error.HTTPError as e:
            print(f"repowire session: daemon error marking offline: {e}", file=sys.stderr)
        except (urllib.error.URLError, OSError):
            pass  # Daemon not running - expected

    return 0


if __name__ == "__main__":
    sys.exit(main())
