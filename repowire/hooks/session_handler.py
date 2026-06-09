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
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote

from repowire.agent_types import AgentType
from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.adapters import normalize
from repowire.hooks.handoff import load_handoff_context, write_handoff_summary
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
from repowire.protocol.capabilities import current_capabilities_metadata
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
    peer_id: str | None = None,
    circle_source: str | None = None,
    pane_id: str | None = None,
    metadata: dict | None = None,
    model: str | None = None,
    role: str | None = None,
    turn_state: str | None = None,
    agent_pid: int | None = None,
    parent_pid: int | None = None,
) -> tuple[str | None, str | None, bool, bool, dict | None]:
    """Register peer via HTTP POST /peers.

    Returns (peer_id, display_name, hijack_rejected, pane_assigned).

    ``hijack_rejected`` is True iff the daemon returned 409 because the
    pane-hijack guard rejected this fresh SessionStart claim (a subprocess
    agent inheriting its parent's TMUX_PANE). The caller should abort
    registration cleanly in that case.

    ``pane_assigned`` is True when this peer owns ``pane_id`` after
    registration, False when the daemon's sticky-orchestrator branch
    refused to displace a live orchestrator. A pane-less peer must skip
    the destructive takeover block (no incumbent ws-hook kill, no prior-
    peer offline mark, no pane runtime metadata rewrite) and must not
    spawn its own ws-hook -- the pane belongs to someone else.
    """
    folder = Path(path).name
    payload: dict = {
        "name": folder,
        "path": path,
        "circle": circle,
        "backend": backend,
    }
    if peer_id:
        payload["peer_id"] = peer_id
    if circle_source:
        payload["circle_source"] = circle_source
    if pane_id:
        payload["pane_id"] = pane_id
    if metadata:
        payload["metadata"] = metadata
    if model:
        payload["model"] = model
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
        return None, None, True, False, None
    if status_code is not None and 200 <= status_code < 300 and result:
        # pane_assigned defaults True for older daemons that don't return it.
        pane_assigned = bool(result.get("pane_assigned", True))
        birth_certificate = result.get("birth_certificate")
        return (
            result.get("peer_id"),
            result.get("display_name"),
            False,
            pane_assigned,
            birth_certificate if isinstance(birth_certificate, dict) else None,
        )
    return None, None, False, False, None


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


def _mark_peer_offline(
    peer_id: str | None,
    *,
    reason: str,
    source: str,
    detail: str | None = None,
) -> None:
    """Best-effort terminal offline with a truthful cause.

    Terminal so the displaced/ended peer's orphan ws-hook cannot reconnect it
    back to life (the daemon rejects retired peer_ids without a live agent).
    """
    if not peer_id:
        return
    daemon_post(
        f"/peers/{quote(peer_id, safe='')}/offline",
        {"reason": reason, "source": source, "detail": detail, "terminal": True},
    )


_CIRCLE_SOURCE_LABELS = {
    "tmux": "from tmux session",
    "spawn_hint": "from spawn hint",
    "fallback": "default fallback",
}


def _find_self_peer(
    peers: list[dict] | None,
    *,
    peer_id: str | None,
    display_name: str,
) -> dict | None:
    """Locate this session's record in a /peers list.

    Prefer peer_id (unique); fall back to display_name when the id is not
    yet known (registration HTTP transport hiccup) or absent from the
    record. Returns None when no record matches — caller falls back to the
    registration/request values.
    """
    if not peers:
        return None
    if peer_id:
        for p in peers:
            if p.get("peer_id") == peer_id:
                return p
    for p in peers:
        if p.get("display_name") == display_name or p.get("name") == display_name:
            return p
    return None


def format_self_context(
    *,
    display_name: str,
    peer_id: str | None,
    circle: str,
    circle_source: str | None,
    backend: str,
    role: str | None,
    cwd: str,
    branch: str | None,
    self_peer: dict | None = None,
) -> str:
    """Render a 'who you are on the mesh' block from daemon registration result.

    When ``self_peer`` is provided (the daemon's record from /peers), its
    effective fields win over the registration/request values for
    display_name, peer_id, circle, backend, role, path, and metadata.branch.
    This matters when the daemon restored circle/role from persisted state
    or canonicalized the display_name — the agent sees what other peers
    will actually see. Request values are only the fallback.
    """
    eff_display_name = display_name
    eff_peer_id = peer_id
    eff_circle = circle
    eff_backend = backend
    eff_model: str | None = None
    eff_role = role
    eff_path = cwd
    eff_branch = branch

    if self_peer:
        eff_display_name = (
            self_peer.get("display_name") or self_peer.get("name") or eff_display_name
        )
        eff_peer_id = self_peer.get("peer_id") or eff_peer_id
        eff_circle = self_peer.get("circle") or eff_circle
        eff_backend = self_peer.get("backend") or eff_backend
        model_value = self_peer.get("model")
        if isinstance(model_value, str) and model_value:
            eff_model = model_value
        # role is an enum on the daemon side; serialized as a string. Empty
        # / missing => keep request fallback.
        peer_role = self_peer.get("role")
        if peer_role:
            eff_role = peer_role
        peer_path = self_peer.get("path")
        if peer_path:
            eff_path = peer_path
        meta_branch = (self_peer.get("metadata") or {}).get("branch")
        if meta_branch:
            eff_branch = meta_branch

    source_label = _CIRCLE_SOURCE_LABELS.get(circle_source or "", circle_source or "")
    circle_str = f"{eff_circle} ({source_label})" if source_label else eff_circle
    project = Path(eff_path).name or eff_display_name

    lines = ["[Repowire Mesh] You are registered on the mesh as:"]
    if eff_peer_id:
        lines.append(f"  - display_name: {eff_display_name}  (peer_id: {eff_peer_id})")
    else:
        lines.append(f"  - display_name: {eff_display_name}")
    lines.append(f"  - circle: {circle_str}")
    lines.append(f"  - backend: {eff_backend}")
    if eff_model:
        lines.append(f"  - model: {eff_model}")
    if eff_role:
        lines.append(f"  - role: {eff_role}")
    lines.append(f"  - project: {project}  (path: {eff_path})")
    if eff_branch:
        lines.append(f"  - branch: {eff_branch}")
    lines.append(
        f"Peers in circle '{eff_circle}' reach you as @{eff_display_name}. "
        "Cross-circle replies only land when the asker used reply_to on an "
        "existing thread."
    )
    return "\n".join(lines)


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
    payload = normalize(input_data, backend)
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
        hint_peer_id = hint.get("peer_id") if hint else None
        if not isinstance(hint_peer_id, str):
            hint_peer_id = None
        # Spawn-seed-drop guard: if the daemon spawned this peer with a seed
        # message, mark turn_state=pending_first_turn so orchestrators can
        # see the brief never landed and re-send via notify_peer. The first
        # UserPromptSubmit transitions this to "working".
        hint_pending_first_turn = bool(hint and hint.get("pending_first_turn"))
        initial_turn_state = "pending_first_turn" if hint_pending_first_turn else None
        metadata: dict = {"project": folder_name, **current_capabilities_metadata()}
        if hook_session_id:
            metadata["hook_session_id"] = hook_session_id
        if payload.model:
            metadata["model_source"] = "hook_session_start"
            metadata["model_observed_at"] = datetime.now(timezone.utc).isoformat()
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
        registration_result = _register_peer_http(
            cwd,
            circle,
            backend_type,
            peer_id=hint_peer_id,
            circle_source=circle_source,
            pane_id=pane_id,
            metadata=metadata,
            model=payload.model,
            role=hint_role,
            turn_state=initial_turn_state,
            agent_pid=agent_pid_val,
            parent_pid=parent_pid_val,
        )
        peer_id, display_name, hijack_rejected, pane_assigned = registration_result[:4]
        birth_certificate = (
            registration_result[4]
            if len(registration_result) > 4 and isinstance(registration_result[4], dict)
            else None
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

        # Sticky orchestrator pane: the daemon registered this peer but
        # refused to give it pane ownership because a live orchestrator
        # holds the pane. Do not touch the incumbent ws-hook, prior-peer
        # status, or pane runtime metadata. The new peer remains usable
        # outbound (MCP/HTTP) but does not own this pane and gets no
        # ws-hook of its own; inbound hook delivery stays with the incumbent.
        if pane_id and not pane_assigned:
            print(
                "repowire: pane "
                f"{pane_id} held by live orchestrator; registered as "
                f"{display_name} ({peer_id}) without pane ownership",
                file=sys.stderr,
            )
            lock_fd.close()
            return 0

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
                _mark_peer_offline(
                    prior_peer_id,
                    reason="pane_takeover",
                    source="session_start_takeover",
                    detail=f"pane {pane_id} taken over by {peer_id}",
                )
            clear_pane_runtime_state(pane_id)

        write_pane_runtime_metadata(
            pane_id,
            {
                "backend": backend,
                "cwd": cwd,
                "display_name": display_name,
                "hook_session_id": hook_session_id,
                "peer_id": peer_id,
                "agent_pid": agent_pid_val,
                "parent_pid": parent_pid_val,
                "birth_certificate": birth_certificate,
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
                agent_pid=agent_pid_val,
            )
        except Exception as e:
            print(f"repowire: failed to start WebSocket hook: {e}", file=sys.stderr)
        finally:
            # Child inherited the flock via pass_fds; release our copy.
            lock_fd.close()

        # Anchor identity from the daemon's effective view of this peer.
        # /peers is the source of truth — the daemon may have restored
        # circle/role from persisted state or canonicalized the display
        # name. Fall back to registration/request values only when the
        # self record isn't available (e.g. daemon unreachable).
        peers = fetch_peers()
        self_peer = _find_self_peer(peers, peer_id=peer_id, display_name=display_name)
        self_context = format_self_context(
            display_name=display_name,
            peer_id=peer_id,
            circle=circle,
            circle_source=circle_source,
            backend=backend,
            role=hint_role,
            cwd=cwd,
            branch=branch,
            self_peer=self_peer,
        )

        peers_context = format_peers_context(peers, display_name) if peers else ""

        handoff_context = load_handoff_context(
            cwd=cwd,
            backend=backend,
            session_id=hook_session_id or None,
        )

        sections = [
            s for s in (self_context, peers_context, handoff_context) if s
        ]
        if sections:
            output = {
                "hookSpecificOutput": {
                    "hookEventName": "SessionStart",
                    "additionalContext": "\n\n".join(sections),
                }
            }
            print(json.dumps(output))

    elif event == "SessionEnd":
        transcript_path = input_data.get("transcript_path")
        write_handoff_summary(
            cwd=cwd,
            backend=backend,
            session_id=hook_session_id or None,
            transcript_path=(
                Path(transcript_path).expanduser().resolve()
                if isinstance(transcript_path, str) and transcript_path else None
            ),
        )
        # SessionEnd fires once at a true session boundary with a reason
        # (verified live: /exit -> prompt_input_exit, /clear -> clear). Skip
        # only /clear — a SessionStart(source=clear) rebinds the same pane
        # ~200ms later and deregistering would race it. Anything else is a
        # quit: deregister explicitly instead of waiting for liveness pings.
        # An unknown/absent reason (other backends) deregisters too — a wrong
        # call self-heals on the next SessionStart, which carries a live
        # agent_pid and reclaims the identity.
        reason = input_data.get("reason", "")
        if reason != "clear":
            end_peer_id = (
                read_pane_runtime_metadata(pane_id).get("peer_id")
                or _get_peer_id_for_pane(pane_id)
            )
            _mark_peer_offline(
                end_peer_id,
                reason="session_end",
                source="session_end_hook",
                detail=f"SessionEnd reason={reason or 'unknown'}",
            )

    return 0


if __name__ == "__main__":
    sys.exit(main())
