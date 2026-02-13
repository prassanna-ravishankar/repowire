"""Async WebSocket hook for Claude Code.

Maintains persistent WebSocket connection to daemon, injects queries via tmux,
and forwards responses via WebSocket.
"""

import asyncio
import json
import logging
import os
import sys
from pathlib import Path

try:
    import libtmux
    import websockets
except ImportError as e:
    print(f"Missing dependency: {e}", file=sys.stderr)
    print("Install with: pip install libtmux websockets", file=sys.stderr)
    sys.exit(1)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def get_circle_from_tmux() -> str:
    """Get circle name from tmux session."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        return "default"

    try:
        server = libtmux.Server()
        pane = server.get_by_id(pane_id)
        if pane and pane.window and pane.window.session:
            return pane.window.session.name
    except Exception as e:
        logger.warning(f"Failed to get circle from tmux: {e}")

    return "default"


def get_display_name_from_cwd() -> str:
    """Get display name from current working directory."""
    cwd = Path.cwd()
    return cwd.name


async def handle_message(data: dict, pane_id: str) -> None:
    """Handle incoming WebSocket message.

    Args:
        data: Message data
        pane_id: Tmux pane ID
    """
    msg_type = data.get("type")

    if msg_type == "query":
        # Inject query via tmux
        try:
            server = libtmux.Server()
            pane = server.get_by_id(pane_id)
            if pane:
                correlation_id = data.get("correlation_id", "")
                from_peer = data.get("from_peer", "unknown")
                text = data.get("text", "")

                # Store correlation_id for later response matching
                response_dir = Path.home() / ".cache" / "repowire" / "correlations"
                response_dir.mkdir(parents=True, exist_ok=True)
                corr_file = response_dir / pane_id.replace("%", "")
                corr_file.write_text(correlation_id)

                # Inject query
                pane.send_keys(text, enter=True)
                pane.send_keys("", enter=True)  # Extra enter

                logger.info(f"Injected query from {from_peer}: {correlation_id[:8]}")
            else:
                logger.error(f"Pane {pane_id} not found")
        except Exception as e:
            logger.error(f"Failed to inject query: {e}")

    elif msg_type == "notify":
        # Inject notification
        try:
            server = libtmux.Server()
            pane = server.get_by_id(pane_id)
            if pane:
                from_peer = data.get("from_peer", "unknown")
                text = data.get("text", "")
                pane.send_keys(f"@{from_peer}: {text}", enter=True)
                pane.send_keys("", enter=True)  # Extra enter
                logger.info(f"Injected notification from {from_peer}")
        except Exception as e:
            logger.error(f"Failed to inject notification: {e}")

    elif msg_type == "broadcast":
        # Inject broadcast
        try:
            server = libtmux.Server()
            pane = server.get_by_id(pane_id)
            if pane:
                from_peer = data.get("from_peer", "unknown")
                text = data.get("text", "")
                pane.send_keys(f"@{from_peer} [broadcast]: {text}", enter=True)
                pane.send_keys("", enter=True)  # Extra enter
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
    pane_file = pane_id.replace("%", "")
    response_file = response_dir / f"{pane_file}.json"

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
                logger.info(
                    f"Forwarded response: {data['correlation_id'][:8]}"
                )
            except Exception as e:
                logger.error(f"Error forwarding response: {e}")
                # Don't delete file if there was an error - try again

        await asyncio.sleep(0.1)  # Poll every 100ms


async def main() -> int:
    """Async hook that maintains WebSocket connection."""
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        logger.error("TMUX_PANE not set")
        return 1

    circle = get_circle_from_tmux()
    display_name = get_display_name_from_cwd()
    path = str(Path.cwd())

    # Get daemon URL from environment or use default
    daemon_host = os.environ.get("REPOWIRE_DAEMON_HOST", "127.0.0.1")
    daemon_port = os.environ.get("REPOWIRE_DAEMON_PORT", "8377")
    uri = f"ws://{daemon_host}:{daemon_port}/ws"

    logger.info(
        f"Starting WebSocket hook for {display_name}@{circle} (pane={pane_id})"
    )

    # Retry connection loop
    while True:
        try:
            async with websockets.connect(uri) as websocket:
                # Send connect message
                await websocket.send(
                    json.dumps(
                        {
                            "type": "connect",
                            "display_name": display_name,
                            "circle": circle,
                            "backend": "claudemux",
                            "path": path,
                        }
                    )
                )

                # Receive session_id
                response = json.loads(await websocket.recv())
                if response.get("type") == "connected":
                    session_id = response["session_id"]
                    logger.info(f"Connected with session_id: {session_id}")

                    # Store session_id in environment for other hooks
                    os.environ["REPOWIRE_SESSION_ID"] = session_id
                else:
                    logger.error(f"Unexpected response: {response}")
                    return 1

                # Start response watcher task
                response_dir = Path.home() / ".cache" / "repowire" / "responses"
                response_dir.mkdir(parents=True, exist_ok=True)

                watcher_task = asyncio.create_task(
                    watch_responses(websocket, response_dir, pane_id)
                )

                try:
                    # Message loop
                    async for message in websocket:
                        data = json.loads(message)
                        await handle_message(data, pane_id)
                finally:
                    watcher_task.cancel()

        except websockets.exceptions.ConnectionClosed:
            logger.warning("Connection closed, reconnecting in 1s...")
            await asyncio.sleep(1)
        except Exception as e:
            logger.error(f"Error: {e}", exc_info=True)
            await asyncio.sleep(1)

    return 0


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        logger.info("Interrupted by user")
        sys.exit(0)
