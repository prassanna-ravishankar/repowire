"""Async WebSocket hook for Claude Code.

Maintains persistent WebSocket connection to daemon, injects queries via tmux,
and forwards responses via WebSocket. Fully reactive — no polling.
"""

import asyncio
import json
import logging
import os
import subprocess
import sys
import time
from typing import cast
from urllib.parse import quote

try:
    import websockets
except ImportError as e:
    print(f"Missing dependency: {e}", file=sys.stderr)
    print("Install with: pip install websockets", file=sys.stderr)
    sys.exit(1)

from repowire.agent_types import AgentType
from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.utils import (
    clear_pane_runtime_state,
    daemon_post,
    get_display_name,
    push_query_cid,
    read_pane_runtime_metadata,
    write_pane_runtime_metadata,
)
from repowire.protocol.capabilities import (
    CURRENT_HOOK_CAPABILITIES,
    CURRENT_HOOK_VERSION,
    PANE_UNSAFE_STRIKE_LIMIT,
)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

# Set once at startup in main() — guards against pane reuse by a different agent
_expected_command: str | None = None
# PID of the process matching _expected_command most recently found in the
# pane's subtree. Written into pane runtime metadata at connect; not a safety
# shortcut (liveness of a pid proves nothing about pane membership).
_cached_agent_pid: int | None = None
# Daemon ping handler tolerates a few consecutive unsafe results to ride out
# transient shell-outs (the agent's foreground briefly becoming git/python/etc.).
# Only after this many in a row does the hook treat the pane as taken over.
_consecutive_ping_unsafe = 0
_RECONNECT_WARNING_ATTEMPTS = 50
_MAX_RECONNECT_DELAY_SECONDS = 30
_AGENT_LIFETIME_CHECK_SECONDS = 5.0


def _format_attachment_lines(attachments: object) -> str:
    """Render attachment metadata into agent-readable fallback text."""
    if not isinstance(attachments, list) or not attachments:
        return ""
    lines = ["", "Attachments:"]
    for item in attachments:
        if not isinstance(item, dict):
            continue
        attachment = cast(dict[str, object], item)
        attachment_id = attachment.get("id")
        path = attachment.get("path")
        label = attachment.get("filename") or path or attachment_id or "attachment"
        target = path or (f"/attachments/{attachment_id}" if attachment_id else "")
        lines.append(f"- {label}: {target}" if target else f"- {label}")
    return "\n".join(lines) if len(lines) > 2 else ""


class PaneUnsafeError(RuntimeError):
    """Raised when the pane no longer belongs to the expected live agent."""


class AgentExitedError(RuntimeError):
    """Raised when the agent process that owns this ws-hook has exited."""


def _parse_positive_int(value: str | None) -> int | None:
    if not value:
        return None
    try:
        parsed = int(value)
    except ValueError:
        return None
    return parsed if parsed > 0 else None


def _pid_exists(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except PermissionError:
        return True
    except ProcessLookupError:
        return False
    except OSError:
        return False


async def _watch_agent_lifetime(agent_pid: int, pane_id: str) -> None:
    """Exit the ws-hook when its owning agent process disappears."""
    while True:
        await asyncio.sleep(_AGENT_LIFETIME_CHECK_SECONDS)
        if not _pid_exists(agent_pid):
            raise AgentExitedError(
                f"Agent pid {agent_pid} for pane {pane_id} exited",
            )


def _push_query_cid(pane_id: str, correlation_id: str) -> None:
    """Append a /query correlation_id to the per-pane FIFO."""
    push_query_cid(pane_id, correlation_id)


def _pane_in_copy_mode(pane_id: str) -> bool:
    """Return True if the pane is in copy-mode (or any other interactive mode).

    `#{pane_in_mode}` is 1 for copy-mode, view-mode, clock-mode, etc. -X cancel
    is the correct exit for all of them, so this broad check is fine.
    """
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_in_mode}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return False
        return result.stdout.strip() == "1"
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False


def _tmux_send_keys(pane_id: str, text: str) -> bool:
    """Send keys to a tmux pane via subprocess.

    Implements Gastown's battle-tested NudgeSession pattern:
    1. If pane is in copy-mode, cancel out first (otherwise -l lands in the
       copy buffer and Enter triggers a selection action instead of submit)
    2. Send text in literal mode (bracketed paste)
    3. 500ms debounce — tested, required for paste to complete
    4. Explicitly close bracketed paste mode with ESC[201~
    5. Enter — submits
    """
    try:
        if _pane_in_copy_mode(pane_id):
            subprocess.run(
                ["tmux", "send-keys", "-t", pane_id, "-X", "cancel"],
                capture_output=True,
                check=True,
            )
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "-l", text],
            capture_output=True,
            check=True,
        )
        time.sleep(0.5)
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "-H", "1b", "5b", "32", "30", "31", "7e"],
            capture_output=True,
            check=True,
        )
        time.sleep(0.1)
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "Enter"],
            capture_output=True,
            check=True,
        )
        return True
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        logger.error(f"Failed to send keys to {pane_id}: {e}")
        return False


def _tmux_display(pane_id: str, fmt: str) -> tuple[str | None, bool]:
    """Query tmux display-message for a pane. Returns (output, conclusive).

    conclusive=True means tmux answered authoritatively — either the value, or
    "no such pane" (non-zero exit / empty output, which tmux produces for
    nonexistent panes). conclusive=False means the check itself failed (tmux
    missing, server busy) and says nothing about the pane.
    """
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", fmt],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None, False
    out = result.stdout.strip()
    if result.returncode != 0 or not out:
        return None, True
    return out, True


def _get_pane_command(pane_id: str) -> str | None:
    """Get the current command running in a tmux pane."""
    cmd, _conclusive = _tmux_display(pane_id, "#{pane_current_command}")
    return cmd.lower() if cmd else None


def _get_pane_pid_checked(pane_id: str) -> tuple[int | None, bool]:
    """Return (pane_pid, conclusive) for a tmux pane."""
    out, conclusive = _tmux_display(pane_id, "#{pane_pid}")
    if out is None:
        return None, conclusive
    return (int(out), True) if out.isdigit() else (None, False)


def _get_pane_pid(pane_id: str) -> int | None:
    """Return the pane's shell PID via tmux. None on failure."""
    pane_pid, _conclusive = _get_pane_pid_checked(pane_id)
    return pane_pid


def _build_ps_child_map() -> tuple[dict[int, list[int]], dict[int, str]] | None:
    """Build (children, pid_to_comm) from one ps shell-out.

    `children` maps {ppid: [pid, ...]}; `pid_to_comm` maps {pid: basename(comm)}.
    Portable across macOS/Linux. `pgrep -P` is non-recursive on macOS so we
    walk the tree ourselves. Returning the comm map alongside lets the BFS
    check the root PID itself (in case the agent has `exec`'d to replace the
    pane shell), not just descendants.
    """
    try:
        result = subprocess.run(
            ["ps", "-axo", "pid=,ppid=,comm="],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return None
    except (FileNotFoundError, subprocess.SubprocessError, subprocess.TimeoutExpired):
        return None

    children: dict[int, list[int]] = {}
    pid_to_comm: dict[int, str] = {}
    for line in result.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        # Format: "  pid ppid comm-with-spaces"
        parts = line.split(None, 2)
        if len(parts) < 3:
            continue
        try:
            pid = int(parts[0])
            ppid = int(parts[1])
        except ValueError:
            continue
        # Login shells report comm with a leading dash ("-zsh"). Strip it or
        # the shell denylist misses them and the pane shell itself gets
        # certified as the agent baseline — the root cause of orphan hooks
        # answering pings "safe" for panes containing nothing but a shell.
        comm = os.path.basename(parts[2].strip()).lower().lstrip("-")
        pid_to_comm[pid] = comm
        children.setdefault(ppid, []).append(pid)
    return children, pid_to_comm


_SHELL_COMMS = frozenset({"bash", "zsh", "sh", "fish", "tcsh", "csh", "dash", "login"})


def _find_agent_in_subtree(
    root_pid: int,
    expected: str,
    children: dict[int, list[int]],
    pid_to_comm: dict[int, str],
) -> int | None:
    """BFS the pane's subtree (including root_pid) for a process matching `expected`.

    Returns the matching PID, or None. `expected` is matched case-insensitively
    against the basename of `comm`. The root PID itself is checked first to
    handle the case where the agent has `exec`'d to replace the pane shell
    (rare but valid; otherwise the agent IS the pane_pid and would be missed).
    """
    target = expected.lower()
    seen: set[int] = set()
    queue: list[int] = [root_pid]
    while queue:
        pid = queue.pop(0)
        if pid in seen:
            continue
        seen.add(pid)
        if pid_to_comm.get(pid) == target:
            return pid
        for child_pid in children.get(pid, []):
            queue.append(child_pid)
    return None


def _capture_baseline_from_subtree(
    root_pid: int,
    children: dict[int, list[int]],
    pid_to_comm: dict[int, str],
) -> str | None:
    """BFS the pane's subtree from `root_pid`, return first non-shell comm.

    Used at hook startup to capture a stable agent baseline that doesn't depend
    on tmux's `pane_current_command`. Tmux reads the kernel's exec name (ucomm),
    while `ps -axo comm` reads BSD comm/argv[0]; these can disagree when an
    agent ships as a per-version binary (e.g. Claude Code v2.1.138 installs at
    ~/.local/share/claude/versions/2.1.138 with a `claude` symlink). Both
    startup and steady-state safety checks must read from the same source —
    `ps -axo comm` — so the baseline is captured here rather than from tmux.

    Returns None if the subtree contains only shell processes (rare: SessionStart
    fired before the agent process exists yet). Caller falls back to the
    historic shell-denylist on the foreground command in that case.
    """
    seen: set[int] = set()
    queue: list[int] = [root_pid]
    while queue:
        pid = queue.pop(0)
        if pid in seen:
            continue
        seen.add(pid)
        comm = pid_to_comm.get(pid)
        if comm and comm not in _SHELL_COMMS:
            return comm
        for child_pid in children.get(pid, []):
            queue.append(child_pid)
    return None


def find_pane_agent_pid(pane_id: str | None) -> int | None:
    """Best-effort pid of the agent process running inside a pane.

    Walks the pane's subtree for the first non-shell process. Used by
    SessionStart when the hook's own parent pid is a shell (codex spawns hook
    commands from the pane shell, so os.getppid() points at zsh, and a watcher
    guarding the shell never notices the agent dying).
    """
    if not pane_id:
        return None
    pane_pid, _conclusive = _get_pane_pid_checked(pane_id)
    if pane_pid is None:
        return None
    ps_result = _build_ps_child_map()
    if ps_result is None:
        return None
    children, pid_to_comm = ps_result
    baseline = _capture_baseline_from_subtree(pane_pid, children, pid_to_comm)
    if baseline is None:
        return None
    return _find_agent_in_subtree(pane_pid, baseline, children, pid_to_comm)


def pid_comm_is_shell(pid: int) -> bool | None:
    """True/False whether `pid`'s command is a plain shell; None if unknowable."""
    try:
        result = subprocess.run(
            ["ps", "-o", "comm=", "-p", str(pid)],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.SubprocessError, OSError):
        return None
    if result.returncode != 0 or not result.stdout.strip():
        return None
    comm = os.path.basename(result.stdout.strip()).lower().lstrip("-")
    return comm in _SHELL_COMMS


def _is_pane_safe(pane_id: str) -> bool | None:
    """Tri-state pane ownership check.

    True: the expected agent is present in the pane's process subtree.
    False: tmux/ps answered authoritatively and the agent is gone (pane closed
    or taken over by something else).
    None: the check itself failed (tmux server busy, ps unavailable) — says
    nothing about the pane. Callers must treat None as "do not act", never as
    "pane is dead": coercing it to False is what caused mass false demotions.

    The pane's foreground command (`#{pane_current_command}`) is unreliable —
    when the agent shells out for a tool call, the foreground briefly becomes
    that subprocess. Walk the pane's subtree from `#{pane_pid}` (the shell)
    instead and look for any descendant whose basename matches the expected
    agent command. There is deliberately no liveness-only cached-pid shortcut:
    a pid being alive proves nothing about it still belonging to this pane,
    and that shortcut let orphan hooks certify dead panes as safe.
    """
    global _cached_agent_pid

    if _expected_command:
        pane_pid, conclusive = _get_pane_pid_checked(pane_id)
        if pane_pid is None:
            return False if conclusive else None
        ps_result = _build_ps_child_map()
        if ps_result is None:
            return None
        children, pid_to_comm = ps_result
        match = _find_agent_in_subtree(pane_pid, _expected_command, children, pid_to_comm)
        if match is None:
            _cached_agent_pid = None
            return False
        _cached_agent_pid = match
        return True

    # No expected baseline: fall back to foreground-command shell denylist so
    # behavior is unchanged for setups that didn't capture a baseline at startup.
    shell_commands = {"bash", "zsh", "sh", "fish", "tcsh", "csh", "dash", "login"}
    cmd, conclusive = _tmux_display(pane_id, "#{pane_current_command}")
    if cmd is None:
        return False if conclusive else None
    return cmd.lower() not in shell_commands


async def handle_message(data: dict, pane_id: str, websocket=None) -> None:
    """Handle incoming WebSocket message.

    Args:
        data: Message data
        pane_id: Tmux pane ID
        websocket: WebSocket connection (for sending error responses)
    """
    msg_type = data.get("type")

    async def _send_delivery_ack(status: str, detail: str | None = None) -> None:
        delivery_id = data.get("delivery_id")
        if msg_type not in {"ask", "notify"} or not websocket or not isinstance(delivery_id, str):
            return
        frame = {
            "type": "delivery_ack",
            "delivery_id": delivery_id,
            "message_type": msg_type,
            "status": status,
        }
        if detail:
            frame["detail"] = detail
        try:
            await websocket.send(json.dumps(frame))
        except Exception:
            pass

    # Safety: verify agent is still running in the pane before injecting text.
    # Injection requires a positive verdict — an inconclusive check (tmux/ps
    # hiccup) must fail the delivery loudly rather than risk typing into a
    # bare shell, but must not kill the hook either.
    needs_safety = msg_type in ("query", "ask", "notify", "broadcast")
    pane_safe = (
        await asyncio.to_thread(_is_pane_safe, pane_id) if needs_safety else True
    )
    if needs_safety and pane_safe is None:
        detail = f"Pane {pane_id} safety inconclusive; delivery not injected"
        logger.error(
            "Inbound delivery dropped: pane %s safety inconclusive for %s "
            "(from=%s to=%s correlation_id=%s)",
            pane_id,
            msg_type,
            data.get("from_peer", "unknown"),
            data.get("to_peer", ""),
            data.get("correlation_id", ""),
        )
        await _send_delivery_ack("failed", detail)
        if msg_type in ("query", "ask") and websocket:
            correlation_id = data.get("correlation_id", "")
            try:
                await websocket.send(
                    json.dumps(
                        {
                            "type": "error",
                            "correlation_id": correlation_id,
                            "error": detail,
                        }
                    )
                )
            except Exception:
                pass
        return
    if needs_safety and pane_safe is False:
        detail = f"Pane {pane_id} not safe for injection"
        logger.error(
            "Inbound delivery dropped: pane %s not safe for %s injection "
            "(from=%s to=%s correlation_id=%s)",
            pane_id,
            msg_type,
            data.get("from_peer", "unknown"),
            data.get("to_peer", ""),
            data.get("correlation_id", ""),
        )
        await _send_delivery_ack("rejected", detail)
        if msg_type in ("query", "ask") and websocket:
            correlation_id = data.get("correlation_id", "")
            try:
                await websocket.send(
                    json.dumps(
                        {
                            "type": "error",
                            "correlation_id": correlation_id,
                            "error": detail,
                        }
                    )
                )
            except Exception:
                pass
        raise PaneUnsafeError(f"Pane {pane_id} no longer matches the expected agent")

    if msg_type == "query":
        correlation_id = data.get("correlation_id", "")
        from_peer = data.get("from_peer", "unknown")
        text = data.get("text", "")
        try:
            if await asyncio.to_thread(_tmux_send_keys, pane_id, text):
                # Track pending correlation_id for stop hook response delivery
                _push_query_cid(pane_id, correlation_id)
                logger.info(f"Injected query from {from_peer}: {correlation_id[:8]}")
            else:
                error_msg = f"Failed to send keys to pane {pane_id}"
                logger.error(error_msg)
                if websocket:
                    await websocket.send(
                        json.dumps(
                            {
                                "type": "error",
                                "correlation_id": correlation_id,
                                "error": error_msg,
                            }
                        )
                    )
                if await asyncio.to_thread(_is_pane_safe, pane_id) is False:
                    raise PaneUnsafeError(error_msg)
        except Exception as e:
            logger.error(f"Failed to inject query: {e}")
            if websocket:
                try:
                    await websocket.send(
                        json.dumps(
                            {
                                "type": "error",
                                "correlation_id": correlation_id,
                                "error": str(e),
                            }
                        )
                    )
                except Exception:
                    pass
            if await asyncio.to_thread(_is_pane_safe, pane_id) is False:
                raise PaneUnsafeError(str(e)) from e

    elif msg_type == "ask":
        # First-class ask: inject framed text. Daemon doesn't track pickup
        # under the simplified model — open asks are surfaced via Stop hook
        # reminders until acked.
        correlation_id = data.get("correlation_id", "")
        from_peer = data.get("from_peer", "unknown")
        to_peer = data.get("to_peer", "")
        text = data.get("text", "") + _format_attachment_lines(data.get("attachments"))
        # `to_peer` is included so a recipient can spot misroutes when two
        # peers share a display_name across circles (issue #136). Older
        # daemons may omit it — keep the legacy frame in that case.
        to_label = f" → @{to_peer}" if to_peer else ""
        injected_text = f"@{from_peer}{to_label} [ask #{correlation_id}]: {text}"
        try:
            if await asyncio.to_thread(_tmux_send_keys, pane_id, injected_text):
                logger.info(
                    f"Injected ask from {from_peer}: {correlation_id[:8]}",
                )
                await _send_delivery_ack("injected")
            else:
                error_msg = f"Failed to send keys to pane {pane_id}"
                logger.error(
                    "Inbound ask dropped: tmux send-keys failed "
                    "(pane=%s from=%s to=%s correlation_id=%s)",
                    pane_id,
                    from_peer,
                    to_peer,
                    correlation_id,
                )
                await _send_delivery_ack("failed", error_msg)
        except Exception as e:
            logger.error(f"Failed to inject ask: {e}")
            await _send_delivery_ack("failed", str(e))

    elif msg_type == "notify":
        # Plain FYI, no lifecycle.
        try:
            from_peer = data.get("from_peer", "unknown")
            to_peer = data.get("to_peer", "")
            text = data.get("text", "") + _format_attachment_lines(data.get("attachments"))
            to_label = f" → @{to_peer}" if to_peer else ""
            if await asyncio.to_thread(
                _tmux_send_keys, pane_id, f"@{from_peer}{to_label}: {text}",
            ):
                logger.info(f"Injected notification from {from_peer}")
                await _send_delivery_ack("injected")
            else:
                error_msg = f"Failed to send keys to pane {pane_id}"
                logger.error(
                    "Inbound notification dropped: tmux send-keys failed "
                    "(pane=%s from=%s to=%s)",
                    pane_id,
                    from_peer,
                    to_peer,
                )
                await _send_delivery_ack("failed", error_msg)
        except Exception as e:
            logger.exception(
                "Inbound notification dropped: injection error "
                "(pane=%s from=%s to=%s): %s",
                pane_id,
                data.get("from_peer", "unknown"),
                data.get("to_peer", ""),
                e,
            )
            await _send_delivery_ack("failed", str(e))

    elif msg_type == "broadcast":
        try:
            from_peer = data.get("from_peer", "unknown")
            text = data.get("text", "")
            msg = f"@{from_peer} [broadcast]: {text}"
            if await asyncio.to_thread(_tmux_send_keys, pane_id, msg):
                logger.info(f"Injected broadcast from {from_peer}")
        except Exception as e:
            logger.error(f"Failed to inject broadcast: {e}")

    elif msg_type == "ping":
        global _consecutive_ping_unsafe
        pane_alive = await asyncio.to_thread(_is_pane_safe, pane_id)
        if websocket:
            try:
                tmux_info = await asyncio.to_thread(get_tmux_info)
                pong: dict[str, object] = {
                    "type": "pong",
                    "circle": tmux_info["session_name"],
                }
                # Omit pane_alive when the check was inconclusive: the daemon
                # treats a missing key as "no verdict" (and old daemons read it
                # as alive, which is the safe direction for a transient failure).
                if pane_alive is not None:
                    pong["pane_alive"] = pane_alive
                await websocket.send(json.dumps(pong))
            except Exception:
                pass
        if pane_alive is True:
            _consecutive_ping_unsafe = 0
            return
        if pane_alive is None:
            # Inconclusive: neither a strike nor a recovery.
            return
        _consecutive_ping_unsafe += 1
        logger.warning(
            "Pane %s reported unsafe on ping (%d/%d consecutive)",
            pane_id,
            _consecutive_ping_unsafe,
            PANE_UNSAFE_STRIKE_LIMIT,
        )
        if _consecutive_ping_unsafe >= PANE_UNSAFE_STRIKE_LIMIT:
            logger.info(f"Pane {pane_id} dead on ping, exiting")
            raise PaneUnsafeError(f"Pane {pane_id} is no longer safe")


async def main() -> int:
    """Async hook that maintains WebSocket connection."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        logger.error("TMUX_PANE not set")
        return 1

    tmux_session_name = get_tmux_info()["session_name"]
    circle = tmux_session_name or "default"
    circle_source = "tmux" if tmux_session_name else "fallback"
    display_name = get_display_name()
    backend_str = os.environ.get("REPOWIRE_BACKEND", "claude-code")
    agent_pid = _parse_positive_int(os.environ.get("REPOWIRE_AGENT_PID"))
    try:
        backend = AgentType(backend_str)
    except ValueError:
        backend = AgentType.CLAUDE_CODE
    path = str(os.getcwd())

    # Snapshot pane command at startup to detect pane reuse. Derive from a
    # subtree walk rather than tmux's pane_current_command so the baseline
    # matches what `ps -axo comm` reports during steady-state safety checks
    # (see _capture_baseline_from_subtree for why these can disagree).
    global _expected_command, _cached_agent_pid
    pane_pid = _get_pane_pid(pane_id)
    ps_result = _build_ps_child_map() if pane_pid is not None else None
    if pane_pid is not None and ps_result is not None:
        children, pid_to_comm = ps_result
        _expected_command = _capture_baseline_from_subtree(pane_pid, children, pid_to_comm)
        if _expected_command:
            _cached_agent_pid = _find_agent_in_subtree(
                pane_pid, _expected_command, children, pid_to_comm,
            )
    if _expected_command is None:
        # Fallback: no agent process found in the subtree yet (SessionStart
        # may have fired before the agent finished spawning). Adopt the
        # foreground command only when it isn't a shell — a shell baseline
        # would make _is_pane_safe match the pane shell itself and certify a
        # dead pane as safe. Left None, the per-check shell-denylist path in
        # _is_pane_safe handles it from here.
        fallback_command = _get_pane_command(pane_id)
        if fallback_command and fallback_command.lstrip("-") not in _SHELL_COMMS:
            _expected_command = fallback_command

    daemon_host = os.environ.get("REPOWIRE_DAEMON_HOST", "127.0.0.1")
    daemon_port = os.environ.get("REPOWIRE_DAEMON_PORT", "8377")
    uri = f"ws://{daemon_host}:{daemon_port}/ws"

    logger.info(f"Starting WebSocket hook for {display_name}@{circle} (pane={pane_id})")

    consecutive_failures = 0
    # Best-known peer identity for the terminal offline call: the env hint
    # until the daemon assigns one, then whatever the last connect returned.
    last_session_id = os.environ.get("REPOWIRE_PEER_ID")

    def _post_agent_exited_offline() -> None:
        if not last_session_id:
            return
        daemon_post(
            f"/peers/{quote(last_session_id, safe='')}/offline",
            {
                "reason": "agent_exited",
                "source": "ws_hook",
                "detail": f"agent pid {agent_pid} for pane {pane_id} exited",
                "terminal": True,
            },
        )

    while True:
        # The in-connection watcher only runs while connected; without this
        # check a hook whose agent died mid-reconnect would retry forever.
        if agent_pid is not None and not _pid_exists(agent_pid):
            logger.info(
                "Agent pid %s exited while disconnected; marking offline and exiting",
                agent_pid,
            )
            await asyncio.to_thread(_post_agent_exited_offline)
            clear_pane_runtime_state(pane_id)
            return 0
        try:
            async with websockets.connect(uri, ping_interval=None, ping_timeout=None) as websocket:
                if consecutive_failures:
                    logger.info(
                        "Reconnected after %d consecutive failed attempts",
                        consecutive_failures,
                    )
                consecutive_failures = 0

                connect_msg: dict[str, object] = {
                    "type": "connect",
                    "display_name": display_name,
                    "circle": circle,
                    "backend": backend,
                    "path": path,
                    "pane_id": pane_id,
                    "circle_source": circle_source,
                    "hook_version": CURRENT_HOOK_VERSION,
                    "capabilities": list(CURRENT_HOOK_CAPABILITIES),
                }
                peer_id = os.environ.get("REPOWIRE_PEER_ID")
                if peer_id:
                    connect_msg["peer_id"] = peer_id
                if agent_pid is not None:
                    # Runtime proof: lets a legitimate reconnect reclaim a
                    # retired peer_id (the daemon verifies the pid is alive).
                    connect_msg["agent_pid"] = agent_pid
                model = os.environ.get("REPOWIRE_MODEL")
                if model:
                    connect_msg["model"] = model
                auth_token = os.environ.get("REPOWIRE_AUTH_TOKEN")
                if auth_token:
                    connect_msg["auth_token"] = auth_token
                await websocket.send(json.dumps(connect_msg))

                response = json.loads(await websocket.recv())
                if response.get("type") == "error" and response.get("code") == "peer_retired":
                    # The daemon terminally offlined this identity and we have
                    # no live agent to justify reclaiming it. Retrying would
                    # just resurrect a zombie — clean up and exit.
                    logger.info(
                        "Peer %s retired by daemon; exiting: %s",
                        peer_id,
                        response.get("error", ""),
                    )
                    clear_pane_runtime_state(pane_id)
                    return 0
                if response.get("type") == "connected":
                    session_id = response["session_id"]
                    last_session_id = session_id
                    logger.info(f"Connected with session_id: {session_id}")
                    metadata = read_pane_runtime_metadata(pane_id)
                    metadata.update({
                        "backend": backend.value,
                        "cwd": path,
                        "display_name": response.get("display_name", display_name),
                        "peer_id": session_id,
                    })
                    if _cached_agent_pid is not None:
                        metadata["agent_pid"] = _cached_agent_pid
                    write_pane_runtime_metadata(pane_id, metadata)
                else:
                    logger.error(f"Unexpected response: {response}, retrying...")
                    await asyncio.sleep(2)
                    continue

                # Message loop — ws-hook stays connected until the daemon
                # disconnects, pane ownership becomes unsafe, or the owning
                # agent pid exits.
                try:
                    async def read_messages() -> None:
                        async for message in websocket:
                            try:
                                data = json.loads(message)
                                await handle_message(data, pane_id, websocket)
                            except PaneUnsafeError:
                                raise
                            except Exception as exc:
                                # A malformed payload or handler bug must not kill
                                # the hook -- Stop-hook respawn is a safety net,
                                # not a routine recovery path. Log and keep reading.
                                logger.exception("Error handling message: %s", exc)

                    if agent_pid is None:
                        await read_messages()
                    else:
                        reader = asyncio.create_task(read_messages())
                        watcher = asyncio.create_task(
                            _watch_agent_lifetime(agent_pid, pane_id),
                        )
                        done, pending = await asyncio.wait(
                            {reader, watcher},
                            return_when=asyncio.FIRST_EXCEPTION,
                        )
                        for task in pending:
                            task.cancel()
                        if pending:
                            await asyncio.gather(*pending, return_exceptions=True)
                        for task in done:
                            task.result()
                except PaneUnsafeError as e:
                    logger.info("%s", e)
                    clear_pane_runtime_state(pane_id)
                    return 0
                except AgentExitedError as e:
                    logger.info("%s", e)
                    # Tell the daemon the truth before exiting — over HTTP, so
                    # it lands even if this websocket just dropped with us.
                    await asyncio.to_thread(_post_agent_exited_offline)
                    clear_pane_runtime_state(pane_id)
                    return 0

        except websockets.exceptions.ConnectionClosed as e:
            consecutive_failures += 1
            delay = min(2 ** (consecutive_failures - 1), _MAX_RECONNECT_DELAY_SECONDS)
            logger.warning(
                "Connection closed (attempt %d): code=%s, reconnecting in %ss...",
                consecutive_failures,
                e.code,
                delay,
            )
            if consecutive_failures == _RECONNECT_WARNING_ATTEMPTS:
                logger.error(
                    "WebSocket hook has failed %d consecutive reconnect attempts and is "
                    "still retrying. Inbound repowire delivery is degraded until the "
                    "daemon becomes reachable again.",
                    consecutive_failures,
                )
            await asyncio.sleep(delay)

        except (websockets.exceptions.WebSocketException, OSError) as e:
            consecutive_failures += 1
            delay = min(2 ** (consecutive_failures - 1), _MAX_RECONNECT_DELAY_SECONDS)
            logger.warning(
                "Connection error (attempt %d): %s, retrying in %ss...",
                consecutive_failures,
                e,
                delay,
            )
            if consecutive_failures == _RECONNECT_WARNING_ATTEMPTS:
                logger.error(
                    "WebSocket hook has failed %d consecutive reconnect attempts and is "
                    "still retrying. Inbound repowire delivery is degraded until the "
                    "daemon becomes reachable again.",
                    consecutive_failures,
                )
            await asyncio.sleep(delay)
            continue

        logger.info("Connection ended, reconnecting in 2s...")
        await asyncio.sleep(2)


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        logger.info("Interrupted by user")
        sys.exit(0)
