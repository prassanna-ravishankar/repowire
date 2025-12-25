"""Relay server for Repowire mesh communication."""

from __future__ import annotations

from datetime import datetime
from typing import Any

import socketio
from fastapi import FastAPI, Depends, HTTPException, Header
from pydantic import BaseModel

from repowire.protocol.peers import Peer, PeerStatus
from repowire.protocol.messages import Message, MessageType
from repowire.relay.auth import validate_api_key, APIKey


class PeerInfo(BaseModel):
    """Extended peer info for relay tracking."""

    peer: Peer
    user_id: str
    sid: str


peers: dict[str, PeerInfo] = {}
user_peers: dict[str, dict[str, str]] = {}
pending_responses: dict[str, str] = {}

sio = socketio.AsyncServer(async_mode="asgi", cors_allowed_origins="*")
app = FastAPI(title="Repowire Relay", version="0.1.0")
socket_app = socketio.ASGIApp(sio, other_asgi_app=app)


def get_user_room(user_id: str) -> str:
    return f"user:{user_id}"


async def get_api_key(x_api_key: str = Header(...)) -> APIKey:
    """Dependency for HTTP endpoint authentication."""
    api_key = validate_api_key(x_api_key)
    if not api_key:
        raise HTTPException(status_code=401, detail="Invalid API key")
    return api_key


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/api/v1/peers")
async def list_peers_http(api_key: APIKey = Depends(get_api_key)) -> list[dict[str, Any]]:
    user_id = api_key.user_id
    if user_id not in user_peers:
        return []
    return [peers[sid].peer.to_dict() for sid in user_peers[user_id].values() if sid in peers]


@sio.event
async def connect(
    sid: str, environ: dict[str, Any], auth: dict[str, Any] | None
) -> bool | dict[str, Any]:
    if not auth or "api_key" not in auth:
        return False

    api_key = validate_api_key(auth["api_key"])
    if not api_key:
        return False

    user_id = api_key.user_id
    await sio.save_session(sid, {"user_id": user_id})
    await sio.enter_room(sid, get_user_room(user_id))

    return {"user_id": user_id}


@sio.event
async def disconnect(sid: str) -> None:
    if sid not in peers:
        return

    peer_info = peers[sid]
    user_id = peer_info.user_id
    peer_name = peer_info.peer.name

    del peers[sid]
    if user_id in user_peers and peer_name in user_peers[user_id]:
        del user_peers[user_id][peer_name]
        if not user_peers[user_id]:
            del user_peers[user_id]

    await sio.emit(
        "peer_left",
        {"name": peer_name},
        room=get_user_room(user_id),
        skip_sid=sid,
    )


@sio.event
async def register(sid: str, data: dict[str, Any]) -> dict[str, Any]:
    session = await sio.get_session(sid)
    user_id = session["user_id"]

    peer = Peer(
        name=data["name"],
        path=data["path"],
        machine=data["machine"],
        status=PeerStatus.ONLINE,
        last_seen=datetime.utcnow(),
        metadata=data.get("metadata", {}),
    )

    peer_info = PeerInfo(peer=peer, user_id=user_id, sid=sid)
    peers[sid] = peer_info

    if user_id not in user_peers:
        user_peers[user_id] = {}
    user_peers[user_id][peer.name] = sid

    await sio.emit(
        "peer_joined",
        peer.to_dict(),
        room=get_user_room(user_id),
        skip_sid=sid,
    )

    return {"status": "registered", "name": peer.name}


@sio.event
async def unregister(sid: str) -> dict[str, str]:
    if sid not in peers:
        return {"status": "not_registered"}

    peer_info = peers[sid]
    user_id = peer_info.user_id
    peer_name = peer_info.peer.name

    del peers[sid]
    if user_id in user_peers and peer_name in user_peers[user_id]:
        del user_peers[user_id][peer_name]

    await sio.emit(
        "peer_left",
        {"name": peer_name},
        room=get_user_room(user_id),
        skip_sid=sid,
    )

    return {"status": "unregistered"}


@sio.event
async def message(sid: str, data: dict[str, Any]) -> dict[str, Any]:
    if sid not in peers:
        return {"error": "not_registered"}

    sender = peers[sid]
    user_id = sender.user_id
    target_name = data.get("to_peer")

    if not target_name:
        return {"error": "missing_target"}

    if user_id not in user_peers or target_name not in user_peers[user_id]:
        return {"error": "peer_not_found", "peer": target_name}

    target_sid = user_peers[user_id][target_name]

    msg = Message(
        type=MessageType(data.get("type", "query")),
        from_peer=sender.peer.name,
        to_peer=target_name,
        payload=data.get("payload", {}),
        correlation_id=data.get("correlation_id"),
    )

    if msg.correlation_id:
        pending_responses[msg.correlation_id] = sid

    await sio.emit("message", msg.to_dict(), to=target_sid)

    return {"status": "sent", "message_id": msg.id}


@sio.event
async def response(sid: str, data: dict[str, Any]) -> dict[str, Any]:
    correlation_id = data.get("correlation_id")
    if not correlation_id:
        return {"error": "missing_correlation_id"}

    if correlation_id not in pending_responses:
        return {"error": "no_pending_request"}

    target_sid = pending_responses.pop(correlation_id)

    if sid not in peers:
        return {"error": "not_registered"}

    sender = peers[sid]

    msg = Message(
        type=MessageType.RESPONSE,
        from_peer=sender.peer.name,
        to_peer=data.get("to_peer"),
        payload=data.get("payload", {}),
        correlation_id=correlation_id,
    )

    await sio.emit("response", msg.to_dict(), to=target_sid)

    return {"status": "sent"}


@sio.event
async def list_peers(sid: str) -> list[dict[str, Any]]:
    session = await sio.get_session(sid)
    user_id = session["user_id"]

    if user_id not in user_peers:
        return []

    return [
        peers[peer_sid].peer.to_dict()
        for peer_sid in user_peers[user_id].values()
        if peer_sid in peers
    ]


def create_app() -> socketio.ASGIApp:
    """Create the ASGI app with Socket.IO mounted."""
    return socket_app
