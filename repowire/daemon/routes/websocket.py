"""WebSocket endpoints for OpenCode plugin connections."""

from __future__ import annotations

import json
import logging
import os
import re
import socket
from typing import TYPE_CHECKING, Any

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from repowire.daemon.deps import get_peer_manager
from repowire.daemon.websocket_manager import get_ws_manager
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.daemon.core import PeerManager
    from repowire.daemon.websocket_manager import WebSocketManager

logger = logging.getLogger(__name__)

router = APIRouter(tags=["websocket"])


@router.websocket("/ws/plugin")
async def plugin_websocket(websocket: WebSocket) -> None:
    """WebSocket endpoint for OpenCode plugin connections.

    Protocol (Plugin → Daemon):
    - register: {type, peer_name, path, metadata}
    - status: {type, status: busy|idle|offline}
    - session: {type, session_id}
    - response: {type, correlation_id, text}
    - error: {type, correlation_id, error}

    Protocol (Daemon → Plugin):
    - registered: {type, ok}
    - query: {type, correlation_id, from_peer, text}
    - notify: {type, from_peer, text}
    - broadcast: {type, from_peer, text}
    """
    await websocket.accept()
    ws_manager = get_ws_manager()
    peer_manager = get_peer_manager()
    peer_name: str | None = None

    try:
        # Wait for registration message
        data = await websocket.receive_json()

        if data.get("type") != "register":
            await websocket.send_json({"type": "error", "error": "First message must be register"})
            await websocket.close()
            return

        peer_name = data.get("peer_name")
        pane_id = data.get("pane_id")
        display_name = data.get("display_name", peer_name)
        path = os.path.normpath(data.get("path", ""))  # Sanitize to prevent path traversal
        metadata = data.get("metadata", {})
        backend = data.get("backend", "opencode")

        if not peer_name or not re.match(r"^[a-zA-Z0-9_-]+$", peer_name):
            await websocket.send_json({"type": "error", "error": "Invalid peer_name format"})
            await websocket.close()
            return

        # Generate pane_id if not provided (for backward compat)
        if not pane_id:
            pane_id = f"opencode:{peer_name}"

        # Register the connection
        await ws_manager.connect(websocket, peer_name, path, metadata)

        # Register with peer manager and config atomically
        peer = Peer(
            pane_id=pane_id,
            display_name=display_name,
            path=path,
            machine=socket.gethostname(),
            backend=backend,
            circle=metadata.get("circle", "global"),
            status=PeerStatus.ONLINE,
            metadata=metadata,
        )
        await peer_manager.register_peer_with_config(
            peer=peer,
            path=path,
            opencode_url=f"ws://plugin/{peer_name}",
            circle=metadata.get("circle"),
        )

        # Send registration confirmation
        await websocket.send_json({"type": "registered", "ok": True})
        logger.info(f"Plugin registered via WebSocket: {peer_name}")

        # Main message loop
        while True:
            data = await websocket.receive_json()
            try:
                await _handle_plugin_message(peer_name, data, ws_manager, peer_manager)
            except Exception as e:
                # Log error but don't kill connection for a single bad message
                logger.error(
                    f"Error handling message from {peer_name}: {e}. "
                    f"Message type: {data.get('type', 'unknown')}",
                    exc_info=True,
                )
                # Notify plugin of error (best effort)
                try:
                    await websocket.send_json(
                        {
                            "type": "error",
                            "error": f"Error processing message: {e}",
                        }
                    )
                except Exception as notify_err:
                    logger.debug(f"Failed to notify plugin {peer_name} of error: {notify_err}")

    except WebSocketDisconnect:
        logger.info(f"Plugin WebSocket disconnected: {peer_name or 'unknown'}")

    except json.JSONDecodeError as e:
        logger.warning(f"Invalid JSON from {peer_name or 'unknown'}: {e}")

    except Exception as e:
        logger.exception(f"Unexpected WebSocket error for {peer_name or 'unknown'}: {e}")

    finally:
        if peer_name:
            await ws_manager.disconnect(peer_name)
            await peer_manager.update_peer_status(peer_name, PeerStatus.OFFLINE)


async def _handle_plugin_message(
    peer_name: str,
    data: dict[str, Any],
    ws_manager: WebSocketManager,
    peer_manager: PeerManager,
) -> None:
    """Handle an incoming message from a plugin.

    Args:
        peer_name: Name of the peer
        data: Message data
        ws_manager: WebSocket manager
        peer_manager: Peer manager
    """
    msg_type = data.get("type")

    if msg_type == "status":
        # Status update from plugin
        status_str = data.get("status", "online")
        status_map = {
            "busy": PeerStatus.BUSY,
            "idle": PeerStatus.ONLINE,
            "online": PeerStatus.ONLINE,
            "offline": PeerStatus.OFFLINE,
        }
        status = status_map.get(status_str, PeerStatus.ONLINE)
        await ws_manager.update_status(peer_name, status)
        await peer_manager.update_peer_status(peer_name, status)

    elif msg_type == "session":
        # Session ID update
        session_id = data.get("session_id")
        if not session_id:
            logger.warning(f"Empty session_id from {peer_name}")
            return
        await ws_manager.update_session_id(peer_name, session_id)
        await peer_manager.update_peer_session_id(peer_name, session_id)

    elif msg_type == "response":
        # Response to a query
        correlation_id = data.get("correlation_id")
        text = data.get("text", "")
        if correlation_id:
            await ws_manager.resolve_query(correlation_id, text)
        else:
            logger.warning(f"Response from {peer_name} missing correlation_id, dropping")

    elif msg_type == "error":
        # Error response to a query
        correlation_id = data.get("correlation_id")
        error = data.get("error", "Unknown error")
        logger.warning(f"Plugin {peer_name} reported error for query {correlation_id}: {error}")
        if correlation_id:
            await ws_manager.resolve_query_error(correlation_id, error)
        else:
            logger.warning(f"Error from {peer_name} missing correlation_id, cannot route")

    elif msg_type == "set_circle":
        # Update peer's circle
        circle = data.get("circle", "global")
        await peer_manager.set_peer_circle(peer_name, circle)

    else:
        logger.warning(f"Unknown message type from {peer_name}: {msg_type}")
