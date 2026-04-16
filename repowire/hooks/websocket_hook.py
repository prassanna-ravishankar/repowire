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
    3. Escape — exits vim INSERT mode if active, harmless otherwise
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
            ["tmux", "send-keys", "-t", pane_id, "Escape"],
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
        )
        if result.returncode != 0:
            return None
        cmd = result.stdout.strip().lower()
        return cmd if cmd else None
    except FileNotFoundError:
        return None


def _is_pane_safe(pane_id: str) -> bool:
    """Check if the tmux pane still has the expected agent process running.

    If _expected_command is set (module-level, captured at startup), the pane
    is only safe when the same command is still running. Falls back to a shell
    denylist otherwise.
    """
    shell_commands = {"bash", "zsh", "sh", "fish", "tcsh", "csh", "dash", "login"}
    cmd = _get_pane_command(pane_id)
    if not cmd:
        return False
    if _expected_command:
        return cmd == _expected_command
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
        if not pane_alive:
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
    global _expected_command
    _expected_command = _get_pane_command(pane_id)

    daemon_host = os.environ.get("REPOWIRE_DAEMON_HOST", "127.0.0.1")
    daemon_port = os.environ.get("REPOWIRE_DAEMON_PORT", "8377")
    uri = f"ws://{daemon_host}:{daemon_port}/ws"

    logger.info(f"Starting WebSocket hook for {display_name}@{circle} (pane={pane_id})")

    max_attempts = 50
    attempt = 0

    while attempt < max_attempts:
        try:
            async with websockets.connect(uri, ping_interval=None, ping_timeout=None) as websocket:
                attempt = 0

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
            attempt += 1
            logger.warning(
                f"Connection closed (attempt {attempt}/{max_attempts}): code={e.code}, "
                f"reconnecting in 2s..."
            )
            await asyncio.sleep(2)

        except (websockets.exceptions.WebSocketException, OSError) as e:
            attempt += 1
            delay = min(1 * 2**attempt, 5)
            logger.warning(
                f"Connection error (attempt {attempt}/{max_attempts}): {e}, retrying in {delay}s..."
            )
            await asyncio.sleep(delay)
            continue

        logger.info("Connection ended, reconnecting in 2s...")
        await asyncio.sleep(2)

    logger.error(f"Exhausted {max_attempts} reconnect attempts, exiting")
    return 1


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        logger.info("Interrupted by user")
        sys.exit(0)
