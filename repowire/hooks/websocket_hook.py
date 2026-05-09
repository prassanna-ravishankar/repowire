"""Async WebSocket hook for Claude Code.

Maintains persistent WebSocket connection to daemon, injects queries via tmux,
and forwards responses via WebSocket. Fully reactive — no polling.
"""

import asyncio
import fcntl
import json
import logging
import os
import subprocess
import sys
import time

try:
    import websockets
except ImportError as e:
    print(f"Missing dependency: {e}", file=sys.stderr)
    print("Install with: pip install websockets", file=sys.stderr)
    sys.exit(1)

from repowire.config.models import AgentType
from repowire.hooks._tmux import get_tmux_info
from repowire.hooks.utils import (
    clear_pane_runtime_state,
    get_display_name,
    pending_cid_path,
    read_pane_runtime_metadata,
    write_pane_runtime_metadata,
)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

# Set once at startup in main() — guards against pane reuse by a different agent
_expected_command: str | None = None
# PID of a process matching _expected_command found in the pane's subtree.
# Cached so the steady-state safety check is os.kill(pid, 0), not a ps shell-out.
# Periodically revalidated via full subtree rescan to defend against PID reuse
# (the hook can run for days; OS PIDs eventually wrap).
_cached_agent_pid: int | None = None
_safety_check_count: int = 0
_FAST_PATH_RESCAN_EVERY = 30
# Daemon ping handler tolerates a few consecutive unsafe results to ride out
# transient shell-outs (the agent's foreground briefly becoming git/python/etc.).
# Only after this many in a row does the hook treat the pane as taken over.
_consecutive_ping_unsafe = 0
_CONSECUTIVE_PANE_UNSAFE_PINGS = 3
_RECONNECT_WARNING_ATTEMPTS = 50
_MAX_RECONNECT_DELAY_SECONDS = 30


class PaneUnsafeError(RuntimeError):
    """Raised when the pane no longer belongs to the expected live agent."""


def _push_pending_cid(pane_id: str, correlation_id: str) -> None:
    """Append a correlation_id to the pending file for a pane.

    Uses flock to prevent race with stop_handler's _pop_pending_cid.
    """
    path = pending_cid_path(pane_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    lock_path = path.with_suffix(path.suffix + ".lock")
    with open(lock_path, "w") as lock_file:
        fcntl.flock(lock_file, fcntl.LOCK_EX)
        try:
            try:
                pending = json.loads(path.read_text()) if path.exists() else []
            except (json.JSONDecodeError, OSError):
                pending = []
            pending.append(correlation_id)
            path.write_text(json.dumps(pending))
        finally:
            fcntl.flock(lock_file, fcntl.LOCK_UN)


def _tmux_send_keys(pane_id: str, text: str) -> bool:
    """Send keys to a tmux pane via subprocess.

    Implements Gastown's battle-tested NudgeSession pattern:
    1. Send text in literal mode (bracketed paste)
    2. 500ms debounce — tested, required for paste to complete
    3. Explicitly close bracketed paste mode with ESC[201~
    4. Enter — submits
    """
    try:
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


def _get_pane_command(pane_id: str) -> str | None:
    """Get the current command running in a tmux pane."""
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_current_command}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return None
        cmd = result.stdout.strip().lower()
        return cmd if cmd else None
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None


def _get_pane_pid(pane_id: str) -> int | None:
    """Return the pane's shell PID via tmux. None on failure."""
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_pid}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return None
        out = result.stdout.strip()
        return int(out) if out.isdigit() else None
    except (FileNotFoundError, ValueError, subprocess.TimeoutExpired):
        return None


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
        comm = os.path.basename(parts[2].strip()).lower()
        pid_to_comm[pid] = comm
        children.setdefault(ppid, []).append(pid)
    return children, pid_to_comm


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


def _is_pane_safe(pane_id: str) -> bool:
    """Check if the pane still has the expected agent in its process subtree.

    The pane's foreground command (`#{pane_current_command}`) is unreliable —
    when the agent shells out for a tool call, the foreground briefly becomes
    that subprocess. Walk the pane's subtree from `#{pane_pid}` (the shell)
    instead and look for any descendant whose basename matches the expected
    agent command. Cached PID is the steady-state fast path.

    Falls back to the historic shell-denylist on the pane's foreground command
    when `_expected_command` is unknown (no agent baseline to compare against).
    """
    global _cached_agent_pid, _safety_check_count
    _safety_check_count += 1

    # Fast path: confirm the previously-found agent PID is still alive.
    # Skipped every _FAST_PATH_RESCAN_EVERY calls so PID reuse can't hide a
    # takeover indefinitely on a long-lived hook.
    rescan_due = (_safety_check_count % _FAST_PATH_RESCAN_EVERY) == 0
    if _cached_agent_pid is not None and not rescan_due:
        try:
            os.kill(_cached_agent_pid, 0)
            return True
        except ProcessLookupError:
            _cached_agent_pid = None
        except PermissionError:
            # Process exists but isn't ours -- agents run as the same user, so
            # EPERM means the cached PID got reused by some system process and
            # we'd be masking a takeover. Drop the cache and rescan.
            _cached_agent_pid = None

    if _expected_command:
        pane_pid = _get_pane_pid(pane_id)
        if pane_pid is None:
            return False
        ps_result = _build_ps_child_map()
        if ps_result is None:
            return False
        children, pid_to_comm = ps_result
        match = _find_agent_in_subtree(pane_pid, _expected_command, children, pid_to_comm)
        if match is None:
            # Drop any cached PID so future fast-path checks don't trust a
            # stale-but-alive process that no longer matches the pane subtree.
            _cached_agent_pid = None
            return False
        _cached_agent_pid = match
        return True

    # No expected baseline: fall back to foreground-command shell denylist so
    # behavior is unchanged for setups that didn't capture a baseline at startup.
    shell_commands = {"bash", "zsh", "sh", "fish", "tcsh", "csh", "dash", "login"}
    cmd = _get_pane_command(pane_id)
    if not cmd:
        return False
    return cmd not in shell_commands


async def handle_message(data: dict, pane_id: str, websocket=None) -> None:
    """Handle incoming WebSocket message.

    Args:
        data: Message data
        pane_id: Tmux pane ID
        websocket: WebSocket connection (for sending error responses)
    """
    msg_type = data.get("type")

    # Safety: verify agent is still running in the pane before injecting text
    needs_safety = msg_type in ("query", "notify", "broadcast")
    if needs_safety and not await asyncio.to_thread(_is_pane_safe, pane_id):
        logger.warning(f"Pane {pane_id} not safe for injection, dropping {msg_type}")
        if msg_type == "query" and websocket:
            correlation_id = data.get("correlation_id", "")
            try:
                await websocket.send(
                    json.dumps(
                        {
                            "type": "error",
                            "correlation_id": correlation_id,
                            "error": f"Pane {pane_id} not safe for injection",
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
                _push_pending_cid(pane_id, correlation_id)
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
                if not await asyncio.to_thread(_is_pane_safe, pane_id):
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
            if not await asyncio.to_thread(_is_pane_safe, pane_id):
                raise PaneUnsafeError(str(e)) from e

    elif msg_type == "notify":
        try:
            from_peer = data.get("from_peer", "unknown")
            text = data.get("text", "")
            if await asyncio.to_thread(_tmux_send_keys, pane_id, f"@{from_peer}: {text}"):
                logger.info(f"Injected notification from {from_peer}")
        except Exception as e:
            logger.error(f"Failed to inject notification: {e}")

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
                await websocket.send(
                    json.dumps(
                        {
                            "type": "pong",
                            "pane_alive": pane_alive,
                            "circle": tmux_info["session_name"],
                        }
                    )
                )
            except Exception:
                pass
        if pane_alive:
            _consecutive_ping_unsafe = 0
            return
        _consecutive_ping_unsafe += 1
        logger.warning(
            "Pane %s reported unsafe on ping (%d/%d consecutive)",
            pane_id,
            _consecutive_ping_unsafe,
            _CONSECUTIVE_PANE_UNSAFE_PINGS,
        )
        if _consecutive_ping_unsafe >= _CONSECUTIVE_PANE_UNSAFE_PINGS:
            logger.info(f"Pane {pane_id} dead on ping, exiting")
            raise PaneUnsafeError(f"Pane {pane_id} is no longer safe")


async def main() -> int:
    """Async hook that maintains WebSocket connection."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        logger.error("TMUX_PANE not set")
        return 1

    circle = get_tmux_info()["session_name"] or "default"
    display_name = get_display_name()
    backend_str = os.environ.get("REPOWIRE_BACKEND", "claude-code")
    try:
        backend = AgentType(backend_str)
    except ValueError:
        backend = AgentType.CLAUDE_CODE
    path = str(os.getcwd())

    # Snapshot pane command at startup to detect pane reuse
    global _expected_command, _cached_agent_pid
    _expected_command = _get_pane_command(pane_id)
    # Pre-populate the cached agent PID so the steady-state safety check is
    # os.kill(pid, 0). Failure here is fine — _is_pane_safe will rebuild on demand.
    if _expected_command:
        pane_pid = _get_pane_pid(pane_id)
        ps_result = _build_ps_child_map() if pane_pid is not None else None
        if pane_pid is not None and ps_result is not None:
            children, pid_to_comm = ps_result
            _cached_agent_pid = _find_agent_in_subtree(
                pane_pid, _expected_command, children, pid_to_comm,
            )

    daemon_host = os.environ.get("REPOWIRE_DAEMON_HOST", "127.0.0.1")
    daemon_port = os.environ.get("REPOWIRE_DAEMON_PORT", "8377")
    uri = f"ws://{daemon_host}:{daemon_port}/ws"

    logger.info(f"Starting WebSocket hook for {display_name}@{circle} (pane={pane_id})")

    consecutive_failures = 0

    while True:
        try:
            async with websockets.connect(uri, ping_interval=None, ping_timeout=None) as websocket:
                if consecutive_failures:
                    logger.info(
                        "Reconnected after %d consecutive failed attempts",
                        consecutive_failures,
                    )
                consecutive_failures = 0

                connect_msg: dict[str, str] = {
                    "type": "connect",
                    "display_name": display_name,
                    "circle": circle,
                    "backend": backend,
                    "path": path,
                    "pane_id": pane_id,
                }
                peer_id = os.environ.get("REPOWIRE_PEER_ID")
                if peer_id:
                    connect_msg["peer_id"] = peer_id
                auth_token = os.environ.get("REPOWIRE_AUTH_TOKEN")
                if auth_token:
                    connect_msg["auth_token"] = auth_token
                await websocket.send(json.dumps(connect_msg))

                response = json.loads(await websocket.recv())
                if response.get("type") == "connected":
                    session_id = response["session_id"]
                    logger.info(f"Connected with session_id: {session_id}")
                    metadata = read_pane_runtime_metadata(pane_id)
                    metadata.update({
                        "backend": backend.value,
                        "cwd": path,
                        "display_name": response.get("display_name", display_name),
                        "peer_id": session_id,
                    })
                    write_pane_runtime_metadata(pane_id, metadata)
                else:
                    logger.error(f"Unexpected response: {response}, retrying...")
                    await asyncio.sleep(2)
                    continue

                # Message loop — fully reactive, no polling tasks
                try:
                    async for message in websocket:
                        data = json.loads(message)
                        await handle_message(data, pane_id, websocket)
                except PaneUnsafeError as e:
                    logger.info("%s", e)
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
