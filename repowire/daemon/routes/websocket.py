"""WebSocket endpoints for OpenCode plugin connections."""

from __future__ import annotations

import logging
import socket
from typing import Any

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from repowire.config.models import load_config
from repowire.daemon.deps import get_peer_manager
from repowire.daemon.websocket_manager import get_ws_manager
from repowire.protocol.peers import Peer, PeerStatus

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
        path = data.get("path", "")
        metadata = data.get("metadata", {})

        if not peer_name:
            await websocket.send_json({"type": "error", "error": "peer_name is required"})
            await websocket.close()
            return

        # Register the connection
        await ws_manager.connect(websocket, peer_name, path, metadata)

        # Also register with peer manager and config for discovery
        config = load_config()
        config.add_peer(
            name=peer_name,
            path=path,
            opencode_url=f"ws://plugin/{peer_name}",  # Marker that this is a WebSocket peer
            circle=metadata.get("circle"),
        )

        # Register with peer manager
        peer = Peer(
            name=peer_name,
            path=path,
            machine=socket.gethostname(),
            circle=metadata.get("circle", "global"),
            status=PeerStatus.ONLINE,
            metadata=metadata,
        )
        await peer_manager.register_peer(peer)

        # Send registration confirmation
        await websocket.send_json({"type": "registered", "ok": True})
        logger.info(f"Plugin registered via WebSocket: {peer_name}")

        # Main message loop
        while True:
            data = await websocket.receive_json()
            await _handle_plugin_message(peer_name, data, ws_manager, peer_manager)

    except WebSocketDisconnect:
        logger.info(f"Plugin WebSocket disconnected: {peer_name or 'unknown'}")

    except Exception as e:
        logger.exception(f"WebSocket error for {peer_name or 'unknown'}: {e}")

    finally:
        if peer_name:
            await ws_manager.disconnect(peer_name)
            await peer_manager.update_peer_status(peer_name, PeerStatus.OFFLINE)


async def _handle_plugin_message(
    peer_name: str,
    data: dict[str, Any],
    ws_manager: Any,
    peer_manager: Any,
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
        if session_id:
            await ws_manager.update_session_id(peer_name, session_id)
            # Also update config
            config = load_config()
            peer_config = config.get_peer(peer_name)
            if peer_config:
                peer_config.session_id = session_id
                config.save()

    elif msg_type == "response":
        # Response to a query
        correlation_id = data.get("correlation_id")
        text = data.get("text", "")
        if correlation_id:
            await ws_manager.resolve_query(correlation_id, text)

    elif msg_type == "error":
        # Error response to a query
        correlation_id = data.get("correlation_id")
        error = data.get("error", "Unknown error")
        if correlation_id:
            await ws_manager.resolve_query_error(correlation_id, error)

    elif msg_type == "set_circle":
        # Update peer's circle
        circle = data.get("circle", "global")
        # Update in peer manager
        peer = await peer_manager.get_peer(peer_name)
        if peer:
            peer.circle = circle
            await peer_manager.register_peer(peer)
        # Update in config
        config = load_config()
        peer_config = config.get_peer(peer_name)
        if peer_config:
            peer_config.circle = circle
            config.save()
        logger.info(f"Peer {peer_name} joined circle: {circle}")

    else:
        logger.warning(f"Unknown message type from {peer_name}: {msg_type}")
