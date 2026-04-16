#!/usr/bin/env python3
"""Handle SessionStart and SessionEnd hooks for auto-registration."""

from __future__ import annotations

import fcntl
import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import quote

from repowire.config.models import AgentType
from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.utils import (
    clear_pane_runtime_state,
    daemon_get,
    daemon_post,
    get_pane_file,
    pane_logs_dir,
    read_pane_runtime_metadata,
    write_pane_runtime_metadata,
    ws_hook_lock_path,
    ws_hook_pid_path,
)


def _register_peer_http(
    path: str,
    circle: str,
    backend: AgentType,
    *,
    pane_id: str | None = None,
    metadata: dict | None = None,
) -> tuple[str | None, str | None]:
    """Register peer via HTTP POST /peers. Returns (peer_id, display_name)."""
    folder = Path(path).name
    payload: dict = {
        "name": folder,
        "path": path,
        "circle": circle,
        "backend": backend,
    }
    if pane_id:
        payload["pane_id"] = pane_id
    if metadata:
        payload["metadata"] = metadata
    result = daemon_post("/peers", payload)
    if result:
        return result.get("peer_id"), result.get("display_name")
    return None, None


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


def _get_peer_id_for_pane(pane_id: str | None) -> str | None:
    """Resolve the current daemon peer_id for a pane, if any."""
    if not pane_id:
        return None
    result = daemon_get(f"/peers/by-pane/{quote(pane_id, safe='')}")
    if result:
        return result.get("peer_id")
    return None


def _mark_peer_offline(peer_id: str | None) -> None:
    """Best-effort offline mark to cancel stale queries before pane takeover."""
    if not peer_id:
        return
    daemon_post(f"/peers/{quote(peer_id, safe='')}/offline", {})


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
        agent = p.get("backend", "claude-code")
        desc = p.get("description", "")
        desc_str = f" - {desc}" if desc else ""
        lines.append(f"  - {p['name']}{branch_str} ({project_name}, {agent}){desc_str}")

    lines.append("")
    lines.append(
        "IMPORTANT: When asked about these projects, ask the peer directly "
        "via ask_peer() rather than searching locally."
    )
    lines.append(
        "Messages from @dashboard or @telegram are from the human user "
        "- treat them like direct instructions. Use notify_peer('telegram', msg) "
        "to send updates to the user's phone."
    )
    lines.append(
        'Call set_description("brief task summary") early - it becomes your '
        "title in the dashboard and peer list."
    )
    lines.append("Peer list may be outdated - use list_peers() to refresh.")
    lines.append(
        "NOTE: SendMessage is a Claude Code harness tool for same-session "
        "teammates only. To reach peers listed above, use repowire tools: "
        "ask_peer(), notify_peer(), broadcast()."
    )

    return "\n".join(lines)


def main(backend: str = "claude-code") -> int:
    """Main entry point."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire session: invalid JSON input: {e}", file=sys.stderr)
        return 0

    event = input_data.get("hook_event_name")
    cwd = input_data.get("cwd", os.getcwd())
    hook_session_id = input_data.get("session_id", "")

    # Convert backend string to AgentType
    try:
        backend_type = AgentType(backend)
    except ValueError:
        backend_type = AgentType.CLAUDE_CODE

    # Get tmux info (pane_id used for tmux targeting)
    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]

    # folder_name is used as metadata.project for human context
    folder_name = get_peer_name(cwd)

    if event == "SessionStart":
        # One ws-hook owns a pane at a time. A repeated SessionStart with the
        # same hook session_id is treated as an ephemeral sub-session of the
        # same live run. Anything else is a real takeover and starts fresh.
        pane_file = get_pane_file(pane_id)
        log_dir = pane_logs_dir()
        lock_path = ws_hook_lock_path(pane_id)
        pid_path = ws_hook_pid_path(pane_id)
        prior_peer_id: str | None = None
        lock_fd = open(lock_path, "w")  # noqa: SIM115
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            old_meta = read_pane_runtime_metadata(pane_id)
            same_live_session = (
                bool(hook_session_id)
                and old_meta.get("hook_session_id") == hook_session_id
                and old_meta.get("cwd") == cwd
                and old_meta.get("backend") == backend
            )
            if same_live_session:
                lock_fd.close()
                return 0

            prior_peer_id = old_meta.get("peer_id") or _get_peer_id_for_pane(pane_id)
            try:
                old_pid = int(pid_path.read_text().strip())
                os.kill(old_pid, signal.SIGTERM)
            except (OSError, ValueError):
                pass
            for _ in range(10):
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except OSError:
                    time.sleep(0.5)
            else:
                try:
                    old_pid = int(pid_path.read_text().strip())
                    os.kill(old_pid, signal.SIGKILL)
                except (OSError, ValueError):
                    pass
                fcntl.flock(lock_fd, fcntl.LOCK_EX)

        if prior_peer_id:
            _mark_peer_offline(prior_peer_id)

        clear_pane_runtime_state(pane_id)

        # Register peer via HTTP -- daemon assigns peer_id and display_name.
        circle = tmux_info["session_name"] or "default"
        metadata = {"project": folder_name}
        peer_id, display_name = _register_peer_http(
            cwd,
            circle,
            backend_type,
            pane_id=pane_id,
            metadata=metadata,
        )
        if not display_name:
            display_name = folder_name  # fallback if daemon unreachable

        write_pane_runtime_metadata(
            pane_id,
            {
                "backend": backend,
                "cwd": cwd,
                "display_name": display_name,
                "hook_session_id": hook_session_id,
                "peer_id": peer_id,
            },
        )

        # Launch async WebSocket hook in background — one per pane.
        try:
            hook_script = Path(__file__).parent / "websocket_hook.py"
            if hook_script.exists():
                log_file = open(log_dir / f"ws-hook-{pane_file}.log", "w")  # noqa: SIM115
                try:
                    env = os.environ.copy()
                    env["REPOWIRE_DISPLAY_NAME"] = display_name
                    if peer_id:
                        env["REPOWIRE_PEER_ID"] = peer_id
                    env["REPOWIRE_BACKEND"] = backend
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
                finally:
                    log_file.close()
                    lock_fd.close()  # child inherits flock; parent releases fd
        except Exception as e:
            print(f"repowire: failed to start WebSocket hook: {e}", file=sys.stderr)

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
