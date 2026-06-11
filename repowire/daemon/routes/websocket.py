"""Unified WebSocket endpoint for all agent types.

Handles Claude Code, OpenCode, and Codex connections via a single WebSocket protocol.
"""

from __future__ import annotations

import asyncio
import hmac
import json
import logging
import os
import socket
from typing import TYPE_CHECKING, Any, cast

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from repowire.agent_types import AgentType
from repowire.daemon.deps import get_app_state
from repowire.daemon.peer_registry import CircleSource, PeerRetiredError
from repowire.daemon.routes._shared import is_valid_identifier
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.peers import PeerRole, PeerStatus, TurnState

if TYPE_CHECKING:
    from repowire.daemon.peer_registry import PeerRegistry
    from repowire.daemon.query_tracker import QueryTracker
    from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)

router = APIRouter(tags=["websocket"])


@router.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket) -> None:
    """Unified WebSocket endpoint for all agent types.

    Protocol (Client -> Daemon):
    - connect: {type, display_name, circle, backend, path?, auth_token?}
    - response: {type, correlation_id, text}    (legacy /query reply)
    - status: {type, status: busy|idle|online, turn_state?}
    - error: {type, correlation_id, error}

    Protocol (Daemon -> Client):
    - connected: {type, session_id}
    - query: {type, correlation_id, from_peer, text}    (legacy blocking RPC)
    - ask: {type, correlation_id, from_peer, text, reply_to?}
        Non-blocking ask. Recipient injects text. Daemon doesn't track
        pickup; open asks reappear in every Stop hook reminder until acked.
    - notify: {type, from_peer, text}    (plain FYI, no lifecycle)
    - broadcast: {type, from_peer, text}
    """
    await websocket.accept()

    state = get_app_state()
    transport: WebSocketTransport = state.transport
    query_tracker: QueryTracker = state.query_tracker
    peer_registry: PeerRegistry = state.peer_registry

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
            if not provided_token or not hmac.compare_digest(
                provided_token, config.daemon.auth_token
            ):
                await websocket.send_json({"type": "error", "error": "Authentication failed"})
                await websocket.close(code=4001, reason="Authentication failed")
                logger.warning("WebSocket connection rejected: invalid or missing auth_token")
                return

        # Extract connection parameters
        display_name = data.get("display_name")
        circle = data.get("circle", "default")

        # Validate circle format (same rules as set_circle handler)
        if not is_valid_identifier(circle):
            await websocket.send_json({"type": "error", "error": "Invalid circle format"})
            await websocket.close(code=4002, reason="Invalid circle")
            return

        backend_str = data.get("backend", "claude-code")
        path = data.get("path")
        tmux_session = data.get("tmux_session")
        pane_id = data.get("pane_id")
        circle_source = data.get("circle_source")
        if circle_source not in (None, "tmux", "spawn_hint", "fallback"):
            await websocket.send_json({"type": "error", "error": "Invalid circle_source"})
            await websocket.close(code=4002, reason="Invalid circle_source")
            return

        # Validate display_name
        if not display_name or not is_valid_identifier(display_name):
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
                    "error": (
                        "Invalid backend: must be claude-code, opencode, codex, "
                        "gemini, antigravity, or pi"
                    ),
                }
            )
            await websocket.close(code=4002, reason="Invalid backend")
            return
        if backend == AgentType.MCP_HTTP:
            # mcp-http is a daemon-owned peer identity for the HTTP MCP mount,
            # not a WebSocket agent runtime.
            await websocket.send_json(
                {"type": "error", "error": "mcp-http is not a WebSocket backend"}
            )
            await websocket.close(code=4002, reason="Invalid backend")
            return

        # Validate role
        role_str = data.get("role", "agent")
        try:
            role = PeerRole(role_str)
        except ValueError:
            valid = ", ".join(r.value for r in PeerRole)
            await websocket.send_json(
                {"type": "error", "error": f"Invalid role: must be one of {valid}"}
            )
            await websocket.close(code=4002, reason="Invalid role")
            return

        # Validate path if provided
        if path:
            normalized_path = os.path.normpath(os.path.abspath(path))
            if normalized_path == "/":
                error_msg = "Invalid path: root directory not allowed"
                await websocket.send_json({"type": "error", "error": error_msg})
                await websocket.close(code=4003, reason="Invalid path")
                logger.warning(f"WebSocket registration rejected: invalid path {path}")
                return
            path = normalized_path

        # Allocate peer_id and register atomically
        # If the client provides a peer_id (ws-hook reconnecting after HTTP
        # pre-registration), the daemon takes over the existing peer.
        claimed_peer_id = data.get("peer_id")
        connect_metadata: dict[str, object] = {}
        hook_version = data.get("hook_version")
        if isinstance(hook_version, int):
            connect_metadata["hook_version"] = hook_version
        capabilities = data.get("capabilities")
        if isinstance(capabilities, list):
            connect_metadata["capabilities"] = [c for c in capabilities if isinstance(c, str)]
        model = data.get("model") if isinstance(data.get("model"), str) else None
        model_details = data.get("model_details")
        if isinstance(model_details, dict):
            connect_metadata["model_details"] = model_details
        # Runtime proof for the retirement guard: a reconnect claiming a
        # retired peer_id is only honored when the owning agent is alive.
        raw_agent_pid = data.get("agent_pid")
        agent_pid = raw_agent_pid if isinstance(raw_agent_pid, int) and raw_agent_pid > 0 else None
        try:
            peer_id, assigned_name = await peer_registry.allocate_and_register(
                circle=circle,
                backend=backend,
                model=model,
                path=path,
                pane_id=pane_id,
                tmux_session=tmux_session,
                machine=os.environ.get("HOSTNAME") or socket.gethostname(),
                role=role,
                peer_id=claimed_peer_id,
                circle_source=cast(CircleSource | None, circle_source),
                metadata=connect_metadata or None,
                agent_pid=agent_pid,
            )
        except PeerRetiredError as exc:
            await websocket.send_json(
                {"type": "error", "code": "peer_retired", "error": str(exc)}
            )
            await websocket.close(code=4004, reason="Peer retired")
            logger.info(
                "WebSocket connect rejected (retired peer_id %s)", claimed_peer_id
            )
            return
        session_id = peer_id

        # Register with transport (handles connection + status tracking)
        await transport.connect(
            session_id,
            websocket,
            pane_id=pane_id,
            display_name=assigned_name,
        )

        # Send connect response with daemon-assigned name
        await websocket.send_json({
            "type": "connected",
            "session_id": session_id,
            "display_name": assigned_name,
        })
        logger.info(f"WebSocket connected: {assigned_name}@{circle} ({session_id}, {backend})")
        asyncio.create_task(
            _flush_queued_notifies(session_id, state),
            name=f"flush-queued-{session_id[:12]}",
        )

        # Message loop
        while True:
            data = await websocket.receive_json()
            try:
                await _handle_message(
                    session_id=session_id,
                    data=data,
                    query_tracker=query_tracker,
                    peer_registry=peer_registry,
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

    except WebSocketDisconnect as e:
        logger.info(f"WebSocket disconnected: {session_id or 'unknown'} (code={e.code})")

    except json.JSONDecodeError as e:
        logger.warning(f"Invalid JSON from {session_id or 'unknown'}: {e}")

    except Exception as e:
        logger.exception(f"Unexpected WebSocket error for {session_id or 'unknown'}: {e}")

    finally:
        if session_id:
            removed = await transport.disconnect(session_id, websocket)
            if removed:
                await query_tracker.cancel_queries_to_peer(session_id)


# How long the queued-notify flush will wait for a spawn seed to land before
# giving up and flushing anyway. The seed and the flush both inject into the
# same pane composer; interleaving them mangles both. We gate on the existing
# `pending_first_turn` marker — set at spawn, cleared once the seeded peer
# takes its first turn — rather than build a general per-pane lock.
_SEED_FLUSH_WAIT_SECONDS = 25.0
_SEED_FLUSH_POLL_SECONDS = 0.5


async def _await_seed_settled(session_id: str, peer_registry: Any) -> Any:
    """Wait while a spawn seed is in flight (peer is ``pending_first_turn``).

    Returns the latest peer snapshot. Bounded by ``_SEED_FLUSH_WAIT_SECONDS``
    so a seed that never gets picked up can't wedge the flush forever; on
    timeout we proceed and flush anyway (logged) — a possible interleave beats
    a permanently stuck queue.
    """
    peer = await peer_registry.get_peer(session_id)
    if peer is None or getattr(peer, "turn_state", None) != "pending_first_turn":
        return peer
    deadline = asyncio.get_event_loop().time() + _SEED_FLUSH_WAIT_SECONDS
    while getattr(peer, "turn_state", None) == "pending_first_turn":
        if asyncio.get_event_loop().time() >= deadline:
            logger.warning(
                "Queued notify flush for %s proceeding while still pending_first_turn "
                "(seed not settled within %.0fs)",
                session_id,
                _SEED_FLUSH_WAIT_SECONDS,
            )
            break
        await asyncio.sleep(_SEED_FLUSH_POLL_SECONDS)
        peer = await peer_registry.get_peer(session_id)
        if peer is None:
            return None
    return peer


async def _flush_queued_notifies(session_id: str, state: Any, *, max_results: int = 50) -> None:
    """Best-effort replay of queued notify/ack rows on fresh WS connect."""
    queue = getattr(state, "queued_delivery_store", None)
    router = getattr(state, "message_router", None)
    peer_registry = getattr(state, "peer_registry", None)
    if queue is None or router is None or peer_registry is None:
        return
    # Hold the flush until any in-flight spawn seed has settled, so the seed
    # and the queued notifies don't interleave into the same pane composer.
    peer = await _await_seed_settled(session_id, peer_registry)
    if peer is None:
        return
    try:
        queued = queue.list_for_peer(session_id, max_results=max_results)
    except AttributeError:
        return
    for delivery in queued:
        if delivery.kind != "notify":
            continue
        try:
            await router.send_notification(
                from_peer=delivery.from_peer_name,
                to_session_id=session_id,
                to_peer_name=peer.display_name,
                text=delivery.text,
                intended_recipient_name=delivery.to_peer_name,
                attachments=delivery.attachments or [],
                delivery_id=delivery.delivery_id,
            )
        except TransportError:
            logger.info(
                "Queued notify replay stopped for %s after transport failure (%s)",
                session_id,
                delivery.delivery_id,
            )
            return
        except Exception:
            logger.warning(
                "Queued notify replay failed for %s (%s)",
                session_id,
                delivery.delivery_id,
                exc_info=True,
            )
            return
        queue.delete(delivery.delivery_id)


async def _handle_message(
    session_id: str,
    data: dict[str, Any],
    query_tracker: QueryTracker,
    peer_registry: PeerRegistry,
) -> None:
    """Handle incoming WebSocket message.

    Args:
        session_id: Session ID
        data: Message data
        query_tracker: Query tracker
        peer_registry: Peer registry (for status/circle/name propagation)
    """
    msg_type = data.get("type")

    if msg_type == "response":
        correlation_id = data.get("correlation_id")
        text = data.get("text", "")
        if not isinstance(text, str):
            logger.warning(f"Response from {session_id} has non-string text, dropping")
            return
        if correlation_id:
            await query_tracker.resolve_query(correlation_id, text)
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
        turn_state = data.get("turn_state")
        if turn_state not in ("idle", "working", "awaiting_input", "pending_first_turn"):
            turn_state = None
        await peer_registry.update_peer_status(session_id, status)
        if turn_state is not None:
            await peer_registry.update_peer_turn_state(
                session_id, cast(TurnState, turn_state),
            )

    elif msg_type == "set_circle":
        new_circle = data.get("circle")
        if new_circle and is_valid_identifier(new_circle):
            await peer_registry.set_peer_circle(session_id, new_circle)
            logger.info(f"Circle updated for {session_id}: {new_circle}")
        elif not new_circle:
            logger.warning(f"set_circle from {session_id} missing circle field")
        else:
            logger.warning(f"set_circle from {session_id} invalid circle format: {new_circle!r}")

    elif msg_type == "update_display_name":
        new_name = data.get("display_name", "")
        if new_name and is_valid_identifier(new_name):
            ok = await peer_registry.update_peer_display_name(session_id, new_name)
            if ok:
                logger.info(f"display_name updated for {session_id}: {new_name}")
            else:
                logger.warning(
                    f"update_display_name from {session_id} rejected:"
                    f" {new_name!r} conflicts with an online peer"
                )
        else:
            logger.warning(f"update_display_name from {session_id} invalid name: {new_name!r}")

    elif msg_type == "pong":
        state = get_app_state()
        state.transport.resolve_pong(session_id, data)

    elif msg_type == "delivery_ack":
        state = get_app_state()
        state.transport.resolve_delivery_ack(session_id, data)

    elif msg_type == "error":
        correlation_id = data.get("correlation_id")
        error = data.get("error", "Unknown error")
        logger.warning(f"Client {session_id} reported error for query {correlation_id}: {error}")
        if correlation_id:
            await query_tracker.resolve_query_error(correlation_id, ValueError(error))
        else:
            logger.warning(f"Error from {session_id} missing correlation_id, cannot route")

    else:
        logger.warning(f"Unknown message type from {session_id}: {msg_type}")
