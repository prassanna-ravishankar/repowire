#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""

from __future__ import annotations

import json
import os
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
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, json.JSONDecodeError):
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
    pane_id: str,
    display_name: str,
    cwd: str,
    machine: str,
    tmux_target: str | None,
    session_id: str,
    metadata: dict,
) -> bool:
    """Register peer with daemon via HTTP.

    Args:
        pane_id: Unique tmux pane ID (e.g., "%42")
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
            "pane_id": pane_id,
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
    except json.JSONDecodeError:
        return 0

    event = input_data.get("hook_event_name")
    session_id = input_data.get("session_id")
    cwd = input_data.get("cwd", os.getcwd())

    # Get tmux info (pane_id is the unique identifier)
    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]
    tmux_target = None
    if tmux_info["session_name"] and tmux_info["window_name"]:
        tmux_target = f"{tmux_info['session_name']}:{tmux_info['window_name']}"

    # display_name is the folder name (human-readable)
    display_name = get_peer_name(cwd)
    machine = get_machine_name()

    if event == "SessionStart":
        # Register peer with daemon (includes git branch in metadata)
        metadata = {}
        branch = get_git_branch(cwd)
        if branch:
            metadata["branch"] = branch

        # pane_id is required for registration
        if pane_id:
            register_peer(
                pane_id=pane_id,
                display_name=display_name,
                cwd=cwd,
                machine=machine,
                tmux_target=tmux_target,
                session_id=session_id,
                metadata=metadata,
            )

        # Fetch peers and output context for Claude
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
        # Notify daemon we're going offline so pending queries get cancelled
        # Use pane_id if available, fallback to display_name for backward compat
        peer_identifier = pane_id if pane_id else display_name
        try:
            req = urllib.request.Request(
                f"{DAEMON_URL}/peers/{peer_identifier}/offline",
                method="POST",
            )
            urllib.request.urlopen(req, timeout=2.0)
        except (urllib.error.URLError, urllib.error.HTTPError, OSError):
            pass  # Best effort - daemon may not be running

    return 0


if __name__ == "__main__":
    sys.exit(main())
