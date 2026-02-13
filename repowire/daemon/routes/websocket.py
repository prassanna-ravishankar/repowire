"""Unified WebSocket endpoint for all backends.

Handles both Claude Code and OpenCode connections via a single WebSocket protocol.
"""

from __future__ import annotations

import json
import logging
import os
import re
from typing import TYPE_CHECKING, Any

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from repowire.config.models import AgentType
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.daemon.query_tracker import QueryTracker
    from repowire.daemon.session_mapper import SessionMapper
    from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)

router = APIRouter(tags=["websocket"])


@router.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket) -> None:
    """Unified WebSocket endpoint for both backends.

    Protocol (Client -> Daemon):
    - connect: {type, display_name, circle, backend, path?, auth_token?}
    - response: {type, correlation_id, text}
    - status: {type, status: busy|idle|online}
    - error: {type, correlation_id, error}

    Protocol (Daemon -> Client):
    - connected: {type, session_id}
    - query: {type, correlation_id, from_peer, text}
    - notify: {type, from_peer, text}
    - broadcast: {type, from_peer, text}
    """
    from repowire.daemon.deps import get_app_state

    await websocket.accept()

    state = get_app_state()
    session_mapper: SessionMapper = state.session_mapper
    transport: WebSocketTransport = state.transport
    query_tracker: QueryTracker = state.query_tracker

    session_id: str | None = None

    try:
        # First message must be connect
        data = await websocket.receive_json()

        if data.get("type") != "connect":
            await websocket.send_json({"type": "error", "error": "First message must be connect"})
            await websocket.close(code=4000, reason="First message must be connect")
            return

        # Authentication check
        config = state.config
        if config.daemon.auth_token:
            provided_token = data.get("auth_token")
            if not provided_token or provided_token != config.daemon.auth_token:
                await websocket.send_json({"type": "error", "error": "Authentication failed"})
                await websocket.close(code=4001, reason="Authentication failed")
                logger.warning("WebSocket connection rejected: invalid or missing auth_token")
                return

        # Extract connection parameters
        display_name = data.get("display_name")
        circle = data.get("circle", "default")
        backend_str = data.get("backend", "claude-code")
        path = data.get("path")
        tmux_session = data.get("tmux_session")

        # Validate display_name
        if not display_name or not re.match(r"^[a-zA-Z0-9_-]+$", display_name):
            await websocket.send_json({"type": "error", "error": "Invalid display_name format"})
            await websocket.close(code=4002, reason="Invalid display_name")
            return

        # Validate against AgentType
        try:
            backend = AgentType(backend_str)
        except ValueError:
            await websocket.send_json(
                {
                    "type": "error",
                    "error": "Invalid backend: must be 'claude-code' or 'opencode'",
                }
            )
            await websocket.close(code=4002, reason="Invalid backend")
            return

        # Validate path if provided
        if path:
            normalized_path = os.path.normpath(os.path.abspath(path))
            home_dir = os.path.expanduser("~")
            if normalized_path == "/" or not normalized_path.startswith(home_dir):
                error_msg = "Invalid path: must be within home directory"
                await websocket.send_json({"type": "error", "error": error_msg})
                await websocket.close(code=4003, reason="Invalid path")
                logger.warning(f"WebSocket registration rejected: invalid path {path}")
                return
            path = normalized_path

        # Register session (reuse if exists)
        session_id = session_mapper.register_session(
            display_name=display_name,
            circle=circle,
            backend=backend,
            path=path,
        )

        # Register with transport (handles connection + status tracking)
        await transport.connect(session_id, websocket)

        # Register with peer manager
        peer = Peer(
            peer_id=session_id,
            display_name=display_name,
            path=path or "",
            machine=os.environ.get("HOSTNAME", "unknown"),
            backend=backend,
            circle=circle,
            status=PeerStatus.ONLINE,
            tmux_session=tmux_session,
            metadata={},
        )
        await state.peer_manager.register_peer(peer)

        # Send connect response
        await websocket.send_json({"type": "connected", "session_id": session_id})
        logger.info(f"WebSocket connected: {display_name}@{circle} ({session_id}, {backend})")

        # Message loop
        while True:
            data = await websocket.receive_json()
            try:
                await _handle_message(
                    session_id=session_id,
                    data=data,
                    transport=transport,
                    query_tracker=query_tracker,
                )
            except Exception as e:
                logger.error(
                    f"Error handling message from {session_id}: {e}. "
                    f"Message type: {data.get('type', 'unknown')}",
                    exc_info=True,
                )
                try:
                    await websocket.send_json(
                        {"type": "error", "error": f"Error processing message: {e}"}
                    )
                except Exception as notify_err:
                    logger.debug(f"Failed to notify {session_id} of error: {notify_err}")

    except WebSocketDisconnect:
        logger.info(f"WebSocket disconnected: {session_id or 'unknown'}")

    except json.JSONDecodeError as e:
        logger.warning(f"Invalid JSON from {session_id or 'unknown'}: {e}")

    except Exception as e:
        logger.exception(f"Unexpected WebSocket error for {session_id or 'unknown'}: {e}")

    finally:
        if session_id:
            await transport.disconnect(session_id)
            query_tracker.cancel_queries_to_peer(session_id)


async def _handle_message(
    session_id: str,
    data: dict[str, Any],
    transport: WebSocketTransport,
    query_tracker: QueryTracker,
) -> None:
    """Handle incoming WebSocket message.

    Args:
        session_id: Session ID
        data: Message data
        transport: WebSocket transport (for status updates)
        query_tracker: Query tracker
    """
    msg_type = data.get("type")

    if msg_type == "response":
        correlation_id = data.get("correlation_id")
        text = data.get("text", "")
        if correlation_id:
            query_tracker.resolve_query(correlation_id, text)
        else:
            logger.warning(f"Response from {session_id} missing correlation_id, dropping")

    elif msg_type == "status":
        status_str = data.get("status", "online")
        status_map = {
            "busy": PeerStatus.BUSY,
            "idle": PeerStatus.ONLINE,
            "online": PeerStatus.ONLINE,
            "offline": PeerStatus.OFFLINE,
        }
        status = status_map.get(status_str, PeerStatus.ONLINE)
        await transport.update_status(session_id, status)

    elif msg_type == "error":
        correlation_id = data.get("correlation_id")
        error = data.get("error", "Unknown error")
        logger.warning(f"Client {session_id} reported error for query {correlation_id}: {error}")
        if correlation_id:
            query_tracker.resolve_query_error(correlation_id, ValueError(error))
        else:
            logger.warning(f"Error from {session_id} missing correlation_id, cannot route")

    else:
        logger.warning(f"Unknown message type from {session_id}: {msg_type}")
