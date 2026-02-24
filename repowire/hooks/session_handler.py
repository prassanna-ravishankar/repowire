#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.utils import get_session_id

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def _is_ws_hook_alive(pane_file: str) -> bool:
    """Check if a ws-hook process is already running for this pane."""
    pid_path = Path.home() / ".cache" / "repowire" / "hooks" / f"{pane_file}.pid"
    if not pid_path.exists():
        return False
    try:
        old_pid = int(pid_path.read_text().strip())
        os.kill(old_pid, 0)  # signal 0 = check process exists
        return True
    except (ProcessLookupError, ValueError, OSError):
        pid_path.unlink(missing_ok=True)
        return False


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
        # Launch async WebSocket hook in background (if not already running)
        pane_file = (pane_id or "unknown").replace("%", "")
        if not _is_ws_hook_alive(pane_file):
            try:
                hook_script = Path(__file__).parent / "websocket_hook.py"
                if hook_script.exists():
                    pid_dir = Path.home() / ".cache" / "repowire" / "hooks"
                    pid_dir.mkdir(parents=True, exist_ok=True)
                    log_dir = Path.home() / ".cache" / "repowire" / "logs"
                    log_dir.mkdir(parents=True, exist_ok=True)
                    log_file = open(log_dir / f"ws-hook-{pane_file}.log", "w")  # noqa: SIM115
                    try:
                        proc = subprocess.Popen(
                            [sys.executable, str(hook_script)],
                            stdout=log_file,
                            stderr=log_file,
                            start_new_session=True,
                            cwd=cwd,
                        )
                    finally:
                        log_file.close()  # Always close — subprocess inherits the fd
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
        # Don't kill the ws-hook process here — SessionEnd can fire spuriously
        # between turns. The ws-hook self-terminates when the pane exits.
        # Just mark the peer offline for pending query cancellation.
        peer_identifier = get_session_id() or display_name

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
