"""Async WebSocket hook for Claude Code.

Maintains persistent WebSocket connection to daemon, injects queries via tmux,
and forwards responses via WebSocket.
"""

import asyncio
import json
import logging
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

try:
    import websockets
except ImportError as e:
    print(f"Missing dependency: {e}", file=sys.stderr)
    print("Install with: pip install websockets", file=sys.stderr)
    sys.exit(1)

from repowire.config.models import AgentType
from repowire.hooks.utils import (
    HOOKS_CACHE_DIR,
    get_display_name,
    get_pane_file,
    get_session_id_from_pane,
)

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
            # Store correlation_id for later response matching
            response_dir = Path.home() / ".cache" / "repowire" / "correlations"
            response_dir.mkdir(parents=True, exist_ok=True)
            corr_file = response_dir / get_pane_file(pane_id)
            corr_file.write_text(correlation_id)

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


async def watch_responses(
    websocket,
    response_dir: Path,
    pane_id: str,
) -> None:
    """Watch for response files and forward via WebSocket.

    Args:
        websocket: WebSocket connection
        response_dir: Directory to watch for response files
        pane_id: Tmux pane ID (for file naming)
    """
    pane_file = get_pane_file(pane_id)
    response_file = response_dir / f"{pane_file}.json"

    max_retries_per_file = 10
    retry_count = 0

    while True:
        if response_file.exists():
            try:
                data = json.loads(response_file.read_text())
                await websocket.send(
                    json.dumps(
                        {
                            "type": "response",
                            "correlation_id": data["correlation_id"],
                            "text": data["response"],
                        }
                    )
                )
                response_file.unlink()
                retry_count = 0
                logger.info(f"Forwarded response: {data['correlation_id'][:8]}")
            except websockets.exceptions.ConnectionClosed:
                logger.warning("WebSocket closed during response forwarding")
                return
            except Exception as e:
                retry_count += 1
                if retry_count >= max_retries_per_file:
                    bad_path = response_file.with_suffix(".json.bad")
                    response_file.rename(bad_path)
                    logger.error(
                        f"Failed to forward response after {max_retries_per_file} retries, "
                        f"moved to {bad_path}: {e}"
                    )
                    retry_count = 0
                else:
                    logger.warning(f"Error forwarding response (retry {retry_count}): {e}")

        await asyncio.sleep(0.1)  # Poll every 100ms


def _mark_peer_offline_http(identifier: str, daemon_url: str) -> None:
    """Best-effort HTTP call to mark peer offline before process exits.

    Called by check_pane_alive so the daemon marks the peer offline even if
    the WebSocket is in a reconnect backoff loop (no active connection to drop).

    identifier: session_id (preferred) or display_name (fallback)
    """
    try:
        req = urllib.request.Request(
            f"{daemon_url}/peers/{identifier}/offline",
            method="POST",
        )
        urllib.request.urlopen(req, timeout=2.0)
    except Exception:
        pass  # Best-effort; daemon may be down


async def check_pane_alive(pane_id: str, display_name: str, daemon_url: str) -> None:
    """Periodically check if the tmux pane still has an agent running.

    Exits the process when the pane is gone or running a bare shell,
    so the ws-hook doesn't outlive its Claude session.
    """
    consecutive_dead = 0
    while True:
        await asyncio.sleep(10)
        if not _is_pane_safe(pane_id):
            consecutive_dead += 1
            if consecutive_dead >= 3:  # 30s of no agent
                logger.info(f"Pane {pane_id} no longer has an agent, exiting")
                # Prefer session_id for unambiguous peer lookup
                identifier = get_session_id_from_pane(pane_id) or display_name
                _mark_peer_offline_http(identifier, daemon_url)
                os._exit(0)
        else:
            consecutive_dead = 0


async def send_heartbeat(websocket, pane_id: str, interval: int = 30) -> None:
    """Send periodic heartbeat status messages to keep daemon in sync.

    This ensures that if SessionEnd marks peer offline but WebSocket stays
    connected (e.g., Claude restarted in same pane), the peer is remarked online.
    """
    while True:
        await asyncio.sleep(interval)
        if _is_pane_safe(pane_id):
            try:
                await websocket.send(json.dumps({"type": "status", "status": "online"}))
            except Exception as e:
                logger.debug(f"Failed to send heartbeat: {e}")
                break  # Connection dead, main loop will reconnect


async def main() -> int:
    """Async hook that maintains WebSocket connection."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        logger.error("TMUX_PANE not set")
        return 1

    # Write own PID for cleanup by SessionEnd
    HOOKS_CACHE_DIR.mkdir(parents=True, exist_ok=True)
    pane_file = get_pane_file(pane_id)
    (HOOKS_CACHE_DIR / f"{pane_file}.pid").write_text(str(os.getpid()))

    circle = get_circle_from_tmux()
    display_name = get_display_name(pane_id)
    path = str(Path.cwd())

    # Get daemon URL from environment or use default
    daemon_host = os.environ.get("REPOWIRE_DAEMON_HOST", "127.0.0.1")
    daemon_port = os.environ.get("REPOWIRE_DAEMON_PORT", "8377")
    daemon_url = f"http://{daemon_host}:{daemon_port}"
    uri = f"ws://{daemon_host}:{daemon_port}/ws"

    logger.info(f"Starting WebSocket hook for {display_name}@{circle} (pane={pane_id})")

    # Start pane liveness checker (self-terminate when agent exits)
    asyncio.create_task(check_pane_alive(pane_id, display_name, daemon_url))

    # Retry connection loop with exponential backoff
    max_attempts = 50
    attempt = 0

    while attempt < max_attempts:
        try:
            async with websockets.connect(uri, ping_interval=None, ping_timeout=None) as websocket:
                attempt = 0  # Reset on successful connection

                # Send connect message
                connect_msg: dict[str, str] = {
                    "type": "connect",
                    "display_name": display_name,
                    "circle": circle,
                    "backend": AgentType.CLAUDE_CODE,
                    "path": path,
                }
                auth_token = os.environ.get("REPOWIRE_AUTH_TOKEN")
                if auth_token:
                    connect_msg["auth_token"] = auth_token
                await websocket.send(json.dumps(connect_msg))

                # Receive session_id
                response = json.loads(await websocket.recv())
                if response.get("type") == "connected":
                    session_id = response["session_id"]
                    logger.info(f"Connected with session_id: {session_id}")

                    # Store session_id for SessionEnd hook to use
                    os.environ["REPOWIRE_SESSION_ID"] = session_id
                    sid_file = HOOKS_CACHE_DIR / f"{pane_file}.sid"
                    sid_file.write_text(session_id)
                else:
                    logger.error(f"Unexpected response: {response}, retrying...")
                    await asyncio.sleep(2)
                    continue

                # Start response watcher task
                response_dir = Path.home() / ".cache" / "repowire" / "responses"
                response_dir.mkdir(parents=True, exist_ok=True)

                watcher_task = asyncio.create_task(
                    watch_responses(websocket, response_dir, pane_id)
                )

                # Start heartbeat task to keep daemon status in sync
                heartbeat_task = asyncio.create_task(
                    send_heartbeat(websocket, pane_id, interval=30)
                )

                try:
                    # Message loop
                    async for message in websocket:
                        data = json.loads(message)
                        await handle_message(data, pane_id, websocket)
                finally:
                    watcher_task.cancel()
                    heartbeat_task.cancel()
                    try:
                        close_code = websocket.close_code
                        close_reason = websocket.close_reason
                        logger.info(f"WS closed: code={close_code} reason={close_reason}")
                    except Exception:
                        pass

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

        # Clean disconnect (no exception) — wait before reconnecting
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
