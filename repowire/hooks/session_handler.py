#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""

from __future__ import annotations

import fcntl
import json
import os
import subprocess
import sys
from pathlib import Path

from repowire.config.models import CACHE_DIR, AgentType
from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.utils import daemon_get, daemon_post, derive_display_name, get_pane_file


def _register_peer_http(
    display_name: str, path: str, circle: str, metadata: dict | None = None
) -> bool:
    """Register peer via HTTP POST /peers (upsert-safe)."""
    payload: dict = {
        "name": display_name,
        "display_name": display_name,
        "path": path,
        "circle": circle,
        "backend": AgentType.CLAUDE_CODE,
    }
    if metadata:
        payload["metadata"] = metadata
    return daemon_post("/peers", payload) is not None


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
    result = daemon_get("/peers")
    if result:
        return result.get("peers", [])
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
        desc = p.get("description", "")
        desc_str = f" — {desc}" if desc else ""
        lines.append(f"  - {p['name']}{branch_str} ({project_name}){desc_str}")

    lines.append("")
    lines.append(
        "IMPORTANT: When asked about these projects, ask the peer directly "
        "via ask_peer() rather than searching locally."
    )
    lines.append(
        "Messages from @dashboard are from a human using the web control plane — "
        "treat them like direct user instructions."
    )
    lines.append(
        'Call set_description("brief task summary") early — it becomes your '
        "title in the dashboard and peer list."
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
    claude_session_id = input_data.get("session_id", "")

    # Get tmux info (pane_id used for tmux targeting)
    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]

    # folder_name is used as metadata.project for human context
    folder_name = get_peer_name(cwd)

    if event == "SessionStart":
        # Ephemeral tool sub-sessions (Haiku-proxied) fire SessionStart too.
        # Skip entirely if a ws-hook is already running for this pane.
        # Uses flock instead of PID probe to avoid TOCTOU races.
        pane_file = get_pane_file(pane_id)
        lock_path = CACHE_DIR / "logs" / f"ws-hook-{pane_file}.lock"
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        lock_fd = open(lock_path, "w")  # noqa: SIM115
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            lock_fd.close()
            return 0  # ws-hook alive → ephemeral sub-session, skip entirely

        # Derive stable name from first 8 chars of Claude's session_id
        display_name = derive_display_name(claude_session_id, cwd)

        # Launch async WebSocket hook in background — one per pane.
        try:
            hook_script = Path(__file__).parent / "websocket_hook.py"
            if hook_script.exists():
                log_dir = CACHE_DIR / "logs"
                log_dir.mkdir(parents=True, exist_ok=True)
                log_file = open(log_dir / f"ws-hook-{pane_file}.log", "w")  # noqa: SIM115
                try:
                    env = os.environ.copy()
                    env["REPOWIRE_DISPLAY_NAME"] = display_name
                    pid_path = CACHE_DIR / "logs" / f"ws-hook-{pane_file}.pid"
                    proc = subprocess.Popen(
                        [sys.executable, str(hook_script)],
                        stdout=log_file,
                        stderr=log_file,
                        start_new_session=True,
                        cwd=cwd,
                        env=env,
                        pass_fds=(lock_fd.fileno(),),
                    )
                    pid_path.write_text(str(proc.pid))
                    lock_fd.close()  # child inherits flock; parent releases fd
                finally:
                    log_file.close()
                    lock_fd.close()
        except Exception as e:
            print(f"repowire: failed to start WebSocket hook: {e}", file=sys.stderr)

        # Register peer via HTTP.
        circle = tmux_info["session_name"] or "default"
        _register_peer_http(display_name, cwd, circle, metadata={"project": folder_name})

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
        # Don't mark peer offline here - SessionEnd fires frequently during
        # agentic loops and tool use cycles, not just at true session end.
        pass

    return 0


if __name__ == "__main__":
    sys.exit(main())
