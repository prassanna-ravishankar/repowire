#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""
from __future__ import annotations

import json
import os
import subprocess
import sys
import urllib.request
import urllib.error
from pathlib import Path

from repowire.config.models import load_config
from repowire.hooks._tmux import get_tmux_target

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
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, json.JSONDecodeError):
        pass
    return None


def format_peers_context(peers: list[dict], my_name: str) -> str:
    """Format peers into context string for Claude."""
    other_peers = [p for p in peers if p["name"] != my_name and p["status"] == "online"]

    if not other_peers:
        return ""

    lines = ["[Repowire Mesh] You have access to other Claude Code sessions working on related projects:"]
    for p in other_peers:
        branch = p.get("metadata", {}).get("branch", "")
        branch_str = f" on {branch}" if branch else ""
        project_name = Path(p.get("path", "")).name or p["name"]
        lines.append(f"  - {p['name']}{branch_str} ({project_name})")

    lines.append("")
    lines.append("IMPORTANT: When asked about these projects, ask the peer directly via ask_peer() rather than searching locally.")
    lines.append("Peer list may be outdated - use list_peers() to refresh.")

    return "\n".join(lines)


def main() -> int:
    """Main entry point."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError:
        return 0

    event = input_data.get("hook_event_name")
    session_id = input_data.get("session_id")
    cwd = input_data.get("cwd", os.getcwd())

    config = load_config()
    tmux_target = get_tmux_target()
    peer_name = get_peer_name(cwd)

    if event == "SessionStart":
        # Register or update peer - name is primary key
        # Include git branch in metadata
        metadata = {}
        branch = get_git_branch(cwd)
        if branch:
            metadata["branch"] = branch

        config.add_peer(
            name=peer_name,
            path=cwd,
            tmux_session=tmux_target,
            session_id=session_id,
            metadata=metadata,
        )

        # Fetch peers and output context for Claude
        peers = fetch_peers()
        if peers:
            context = format_peers_context(peers, peer_name)
            if context:
                output = {
                    "hookSpecificOutput": {
                        "hookEventName": "SessionStart",
                        "additionalContext": context,
                    }
                }
                print(json.dumps(output))

    elif event == "SessionEnd":
        # On session end, just clear the session_id but keep the peer
        # The daemon will clean up stale peers based on tmux status
        if session_id:
            config.update_peer_session(peer_name, "")

    return 0


if __name__ == "__main__":
    sys.exit(main())
