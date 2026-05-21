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
    daemon_post_with_status,
    read_pane_runtime_metadata,
    write_pane_runtime_metadata,
    ws_hook_lock_path,
    ws_hook_pid_path,
)
from repowire.hooks.ws_hook_supervisor import spawn_ws_hook
from repowire.peer_describe import compute_git_status
from repowire.spawn_hints import consume_hint_full


def _read_ppid_of(pid: int) -> int | None:
    """Return the parent pid of ``pid``, or None if it can't be determined.

    Used by the pane-hijack guard payload: the hook's ppid is the agent
    process; the agent's ppid is what tells us whether the agent was itself
    spawned by another mesh peer (a hijack) or by a plain shell (legitimate).
    """
    try:
        result = subprocess.run(
            ["ps", "-o", "ppid=", "-p", str(pid)],
            capture_output=True,
            text=True,
            timeout=2,
        )
    except Exception:
        # Best-effort: any failure (subprocess error, missing binary, OS
        # error, or in tests where subprocess.Popen is mocked) → unknown
        # parent. The guard treats parent_pid=None as "can't decide" and
        # lets the claim through, which is the safe default.
        return None
    if result.returncode != 0:
        return None
    try:
        return int(result.stdout.strip())
    except (ValueError, AttributeError):
        return None


def _register_peer_http(
    path: str,
    circle: str,
    backend: AgentType,
    *,
    circle_source: str | None = None,
    pane_id: str | None = None,
    metadata: dict | None = None,
    role: str | None = None,
    turn_state: str | None = None,
    agent_pid: int | None = None,
    parent_pid: int | None = None,
) -> tuple[str | None, str | None, bool]:
    """Register peer via HTTP POST /peers.

    Returns (peer_id, display_name, hijack_rejected). hijack_rejected is True
    iff the daemon returned 409 because the pane-hijack guard rejected this
    fresh SessionStart claim (a subprocess agent inheriting its parent's
    TMUX_PANE). The caller should abort registration cleanly in that case.
    """
    folder = Path(path).name
    payload: dict = {
        "name": folder,
        "path": path,
        "circle": circle,
        "backend": backend,
    }
    if circle_source:
        payload["circle_source"] = circle_source
    if pane_id:
        payload["pane_id"] = pane_id
    if metadata:
        payload["metadata"] = metadata
    if role:
        payload["role"] = role
    if turn_state:
        payload["turn_state"] = turn_state
    if agent_pid is not None:
        payload["agent_pid"] = agent_pid
    if parent_pid is not None:
        payload["parent_pid"] = parent_pid
    status_code, result = daemon_post_with_status("/peers", payload)
    if status_code == 409:
        detail = (result or {}).get("detail", "")
        print(
            f"repowire: SessionStart rejected by daemon pane-hijack guard: {detail}",
            file=sys.stderr,
        )
        return None, None, True
    if status_code is not None and 200 <= status_code < 300 and result:
        return result.get("peer_id"), result.get("display_name"), False
    return None, None, False


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
        "via ask() rather than searching locally. Use ask() for tracked work "
        "that needs explicit ack. ask() is non-blocking and "
        "returns a correlation_id; the peer responds via ack(corr_id) or "
        "ack(corr_id, message). Use ask(reply_to=corr_id, ...) to chain a "
        "follow-up that closes the prior thread. Asking yourself is valid "
        "for deliberate loopback checks, but use notify_peer for "
        "self-wakes/reminders."
    )
    lines.append(
        "Messages from @dashboard or @telegram are from the human user "
        "- treat them like direct instructions. Use notify_peer('telegram', msg) "
        "to send updates to the user's phone."
    )
    lines.append(
        "Inbound asks arrive framed as `@peer [ask #corr_id]: ...` -- you "
        "MUST close them with ack(corr_id) (bare seen-no-action) or "
        "ack(corr_id, message) (reply). Otherwise repowire will inject a "
        "reminder on your next turn. Inbound replies arrive as "
        "`[ack #corr_id from @peer] message` -- those are closures, no "
        "ack needed."
    )
    lines.append(
        'Call set_description("brief task summary") early - it becomes your '
        "title in the dashboard and peer list."
    )
    lines.append("Peer list may be outdated - use list_peers() to refresh.")
    lines.append(
        "NOTE: SendMessage is a Claude Code harness tool for same-session "
        "teammates only. To reach peers listed above, use repowire tools: "
        "ask(), ack(), notify_peer(), broadcast()."
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
        lock_path = ws_hook_lock_path(pane_id)
        pid_path = ws_hook_pid_path(pane_id)
        prior_peer_id: str | None = None
        needs_takeover = False
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
            # Real takeover-or-hijack scenario. Defer the destructive parts
            # (killing the incumbent ws-hook, marking the prior peer offline,
            # clearing pane runtime state) until AFTER the daemon accepts our
            # registration. A rejected hijack (#190) must leave the incumbent
            # untouched.
            needs_takeover = True

        # Register peer via HTTP -- daemon assigns peer_id and display_name.
        # Codex strips tmux env from hook subprocesses, so fall back to the
        # spawn hint before defaulting.
        hint = consume_hint_full(cwd, backend)
        if tmux_info["session_name"]:
            circle = tmux_info["session_name"]
            circle_source = "tmux"
        elif hint:
            circle = hint["circle"]
            circle_source = "spawn_hint"
        else:
            circle = "default"
            circle_source = "fallback"
        hint_role = hint.get("role") if hint else None
        # Spawn-seed-drop guard: if the daemon spawned this peer with a seed
        # message, mark turn_state=pending_first_turn so orchestrators can
        # see the brief never landed and re-send via notify_peer. The first
        # UserPromptSubmit transitions this to "working".
        hint_pending_first_turn = bool(hint and hint.get("pending_first_turn"))
        initial_turn_state = "pending_first_turn" if hint_pending_first_turn else None
        metadata: dict = {"project": folder_name}
        if hook_session_id:
            metadata["hook_session_id"] = hook_session_id
        branch = get_git_branch(cwd)
        if branch:
            metadata["branch"] = branch
        git_status = compute_git_status(cwd)
        if git_status is not None:
            metadata["git_status"] = git_status
        # Pid lineage for the pane-hijack guard:
        #   - agent_pid: the AGENT process that owns this hook == os.getppid().
        #     The hook process itself dies seconds later, so its own pid is
        #     useless for after-the-fact identity checks.
        #   - parent_pid: the AGENT's parent. For a legitimately launched
        #     agent that's a shell; for a subprocess agent (e.g. gemini
        #     invoked by a still-running claude), it's the parent agent's
        #     pid, which will match the existing peer's recorded agent_pid
        #     and trip the guard.
        agent_pid_val = os.getppid()
        parent_pid_val = _read_ppid_of(agent_pid_val)
        peer_id, display_name, hijack_rejected = _register_peer_http(
            cwd,
            circle,
            backend_type,
            circle_source=circle_source,
            pane_id=pane_id,
            metadata=metadata,
            role=hint_role,
            turn_state=initial_turn_state,
            agent_pid=agent_pid_val,
            parent_pid=parent_pid_val,
        )
        if hijack_rejected:
            # Daemon rejected this pane claim. Don't touch the incumbent's
            # ws-hook, prior-peer status, or pane runtime metadata — the
            # rejection must leave the world unchanged (issue #190).
            lock_fd.close()
            return 0
        registration_accepted = peer_id is not None
        if needs_takeover and not registration_accepted:
            # Hijack-candidate path: the daemon didn't reject (no 409), but
            # also didn't confirm acceptance (transport error, 5xx, etc.).
            # Without a confirmed accept we can't justify tearing down the
            # incumbent's ws-hook / pane metadata — that would leave a
            # half-broken pane on every daemon hiccup. Bail cleanly.
            print(
                "repowire: registration unconfirmed during pane takeover, "
                "leaving incumbent in place",
                file=sys.stderr,
            )
            lock_fd.close()
            return 0
        if not display_name:
            display_name = folder_name  # fallback if daemon unreachable

        # Daemon accepted our claim. NOW perform the destructive takeover
        # steps: evict the incumbent ws-hook, mark its peer offline, clear
        # its pane runtime state, then claim the flock for ourselves.
        if needs_takeover:
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

            if prior_peer_id and prior_peer_id != peer_id:
                _mark_peer_offline(prior_peer_id)
            clear_pane_runtime_state(pane_id)

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
            spawn_ws_hook(
                pane_id=pane_id,
                peer_id=peer_id,
                display_name=display_name,
                backend=backend,
                cwd=cwd,
                lock_fd=lock_fd,
            )
        except Exception as e:
            print(f"repowire: failed to start WebSocket hook: {e}", file=sys.stderr)
        finally:
            # Child inherited the flock via pass_fds; release our copy.
            lock_fd.close()

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
