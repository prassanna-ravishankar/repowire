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

try:
    import websockets
except ImportError as e:
    print(f"Missing dependency: {e}", file=sys.stderr)
    print("Install with: pip install websockets", file=sys.stderr)
    sys.exit(1)

from repowire.config.models import AgentType
from repowire.hooks.utils import get_display_name

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def get_circle_from_tmux() -> str:
    """Get circle name from tmux session."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        return "default"

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{session_name}"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0 and result.stdout.strip():
            return result.stdout.strip()
    except Exception as e:
        logger.warning(f"Failed to get circle from tmux: {e}")

    return "default"


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


def _is_pane_safe(pane_id: str) -> bool:
    """Check if the tmux pane still has an AI agent process running.

    Uses a denylist of known shells rather than an allowlist of agent binaries,
    because agent CLIs may report version strings (e.g. "2.1.45") as
    pane_current_command instead of their binary name.
    """
    shell_commands = {"bash", "zsh", "sh", "fish", "tcsh", "csh", "dash", "login"}
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_current_command}"],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            return False
        cmd = result.stdout.strip().lower()
        if not cmd:
            return False  # pane doesn't exist; tmux exits 0 with empty stdout
        return cmd not in shell_commands
    except (subprocess.CalledProcessError, FileNotFoundError):
        return False


async def handle_message(data: dict, pane_id: str, websocket=None) -> None:
    """Handle incoming WebSocket message.

    Args:
        data: Message data
        pane_id: Tmux pane ID
        websocket: WebSocket connection (for sending error responses)
    """
    msg_type = data.get("type")

    # Safety: verify agent is still running in the pane before injecting text
    if msg_type in ("query", "notify", "broadcast") and not _is_pane_safe(pane_id):
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
        return

    if msg_type == "query":
        correlation_id = data.get("correlation_id", "")
        from_peer = data.get("from_peer", "unknown")
        text = data.get("text", "")
        try:
            if _tmux_send_keys(pane_id, text):
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

    elif msg_type == "notify":
        try:
            from_peer = data.get("from_peer", "unknown")
            text = data.get("text", "")
            if _tmux_send_keys(pane_id, f"@{from_peer}: {text}"):
                logger.info(f"Injected notification from {from_peer}")
        except Exception as e:
            logger.error(f"Failed to inject notification: {e}")

    elif msg_type == "broadcast":
        try:
            from_peer = data.get("from_peer", "unknown")
            text = data.get("text", "")
            if _tmux_send_keys(pane_id, f"@{from_peer} [broadcast]: {text}"):
                logger.info(f"Injected broadcast from {from_peer}")
        except Exception as e:
            logger.error(f"Failed to inject broadcast: {e}")

    elif msg_type == "ping":
        pane_alive = _is_pane_safe(pane_id)
        if websocket:
            try:
                await websocket.send(
                    json.dumps(
                        {
                            "type": "pong",
                            "pane_alive": pane_alive,
                        }
                    )
                )
            except Exception:
                pass
        if not pane_alive:
            logger.info(f"Pane {pane_id} dead on ping, exiting")
            os._exit(0)


async def main() -> int:
    """Async hook that maintains WebSocket connection."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        logger.error("TMUX_PANE not set")
        return 1

    circle = get_circle_from_tmux()
    display_name = get_display_name()
    path = str(os.getcwd())

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
                    "backend": AgentType.CLAUDE_CODE,
                    "path": path,
                    "pane_id": pane_id,
                }
                auth_token = os.environ.get("REPOWIRE_AUTH_TOKEN")
                if auth_token:
                    connect_msg["auth_token"] = auth_token
                await websocket.send(json.dumps(connect_msg))

                response = json.loads(await websocket.recv())
                if response.get("type") == "connected":
                    session_id = response["session_id"]
                    logger.info(f"Connected with session_id: {session_id}")
                else:
                    logger.error(f"Unexpected response: {response}, retrying...")
                    await asyncio.sleep(2)
                    continue

                # Message loop — fully reactive, no polling tasks
                async for message in websocket:
                    data = json.loads(message)
                    await handle_message(data, pane_id, websocket)

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
