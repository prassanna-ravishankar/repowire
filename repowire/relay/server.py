"""Relay server for cross-machine Repowire daemon-to-daemon communication.

Provides:
- WebSocket bridge: daemons connect via /ws/relay, messages forwarded within user scope
- HTTP tunnel: browser requests to /d/{token}/... are proxied to a connected daemon
- Landing page: minimal UI at / for entering an API key to access a remote dashboard
"""

from __future__ import annotations

import asyncio
import base64
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

from fastapi import Depends, FastAPI, Header, HTTPException, Request, WebSocket, WebSocketDisconnect
from fastapi.responses import HTMLResponse, Response
from pydantic import BaseModel

from repowire.relay.auth import APIKey, register_token, validate_api_key

log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Data model
# ---------------------------------------------------------------------------

HTTP_TUNNEL_TIMEOUT = 30  # seconds


@dataclass
class DaemonConnection:
    user_id: str
    daemon_id: str
    websocket: WebSocket
    connected_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


# Registry
_connections: dict[str, DaemonConnection] = {}  # key: "{user_id}/{daemon_id}"
_user_daemons: dict[str, set[str]] = {}  # user_id -> set of connection keys
_http_futures: dict[str, asyncio.Future[dict[str, Any]]] = {}  # request_id -> Future

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _conn_key(user_id: str, daemon_id: str) -> str:
    return f"{user_id}/{daemon_id}"


def _register(conn: DaemonConnection) -> None:
    key = _conn_key(conn.user_id, conn.daemon_id)
    _connections[key] = conn
    _user_daemons.setdefault(conn.user_id, set()).add(key)
    log.info("Daemon connected: %s (user=%s)", conn.daemon_id, conn.user_id)


def _unregister(conn: DaemonConnection) -> None:
    key = _conn_key(conn.user_id, conn.daemon_id)
    _connections.pop(key, None)
    keys = _user_daemons.get(conn.user_id)
    if keys:
        keys.discard(key)
        if not keys:
            del _user_daemons[conn.user_id]
    # Fail-fast any pending HTTP tunnel requests for this daemon
    cancelled = 0
    for req_id, future in list(_http_futures.items()):
        if not future.done():
            future.set_exception(ConnectionError("Daemon disconnected"))
            cancelled += 1
    if cancelled:
        log.info("Cancelled %d pending tunnel requests for %s", cancelled, conn.daemon_id)
    log.info("Daemon disconnected: %s (user=%s)", conn.daemon_id, conn.user_id)


def _get_daemon(user_id: str, daemon_id: str) -> DaemonConnection | None:
    return _connections.get(_conn_key(user_id, daemon_id))


def _get_any_daemon(user_id: str) -> DaemonConnection | None:
    """Return the first connected daemon for a user, or None."""
    keys = _user_daemons.get(user_id)
    if not keys:
        return None
    for key in keys:
        conn = _connections.get(key)
        if conn:
            return conn
    return None


def _get_all_daemons(user_id: str) -> list[DaemonConnection]:
    """Return all connected daemons for a user."""
    keys = _user_daemons.get(user_id, set())
    return [_connections[k] for k in keys if k in _connections]


async def _forward_to_daemon(conn: DaemonConnection, message: dict[str, Any]) -> None:
    try:
        await conn.websocket.send_json(message)
    except Exception:
        log.warning("Failed to forward message to %s/%s", conn.user_id, conn.daemon_id)


# ---------------------------------------------------------------------------
# Auth dependency
# ---------------------------------------------------------------------------


async def get_api_key(x_api_key: str = Header(...)) -> APIKey:
    api_key = validate_api_key(x_api_key)
    if not api_key:
        raise HTTPException(status_code=401, detail="Invalid API key")
    return api_key


# ---------------------------------------------------------------------------
# Landing page HTML
# ---------------------------------------------------------------------------

_LANDING_HTML = """\
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Repowire Relay</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    background: #0a0a0f;
    color: #c8c8d0;
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 100vh;
  }
  .container { text-align: center; max-width: 400px; padding: 2rem; }
  h1 { color: #e0e0e8; font-size: 1.6rem; margin-bottom: 0.3rem; }
  .subtitle { color: #6a6a7a; font-size: 0.85rem; margin-bottom: 2rem; }
  form { display: flex; gap: 0.5rem; }
  input {
    flex: 1;
    padding: 0.6rem 0.8rem;
    background: #14141f;
    border: 1px solid #2a2a3a;
    border-radius: 6px;
    color: #e0e0e8;
    font-family: monospace;
    font-size: 0.9rem;
    outline: none;
  }
  input:focus { border-color: #4a4a6a; }
  input::placeholder { color: #3a3a4a; }
  button {
    padding: 0.6rem 1.2rem;
    background: #1a1a2f;
    border: 1px solid #2a2a3a;
    border-radius: 6px;
    color: #c8c8d0;
    cursor: pointer;
    font-size: 0.9rem;
  }
  button:hover { background: #22223a; border-color: #4a4a6a; }
</style>
</head>
<body>
<div class="container">
  <h1>repowire relay</h1>
  <p class="subtitle">Enter your API key to access your dashboard</p>
  <form onsubmit="go(event)">
    <input id="key" type="text" placeholder="rw_..." autocomplete="off" spellcheck="false">
    <button type="submit">Go</button>
  </form>
</div>
<script>
function go(e) {
  e.preventDefault();
  var k = document.getElementById("key").value.trim();
  if (k) window.location.href = "/d/" + encodeURIComponent(k) + "/dashboard";
}
</script>
</body>
</html>
"""

# ---------------------------------------------------------------------------
# WebSocket message handlers
# ---------------------------------------------------------------------------


async def _handle_targeted_forward(conn: DaemonConnection, msg: dict[str, Any]) -> None:
    """Forward a message to a specific daemon by target_daemon_id."""
    msg_type = msg.get("type", "?")
    target_id = msg.get("target_daemon_id")
    if not target_id:
        log.warning("%s missing target_daemon_id from %s", msg_type, conn.daemon_id)
        return
    target = _get_daemon(conn.user_id, target_id)
    if not target:
        log.warning("%s target %s not connected (user=%s)", msg_type, target_id, conn.user_id)
        return
    msg["source_daemon_id"] = conn.daemon_id
    await _forward_to_daemon(target, msg)


async def _handle_relay_broadcast(conn: DaemonConnection, msg: dict[str, Any]) -> None:
    msg["source_daemon_id"] = conn.daemon_id
    for target in _get_all_daemons(conn.user_id):
        if target.daemon_id != conn.daemon_id:
            await _forward_to_daemon(target, msg)


async def _handle_http_response(msg: dict[str, Any]) -> None:
    request_id = msg.get("request_id")
    if not request_id:
        return
    future = _http_futures.get(request_id)
    if future and not future.done():
        future.set_result(msg)


_MSG_HANDLERS: dict[str, Any] = {
    "relay_query": _handle_targeted_forward,
    "relay_notify": _handle_targeted_forward,
    "relay_broadcast": _handle_relay_broadcast,
    "relay_response": _handle_targeted_forward,
}

# ---------------------------------------------------------------------------
# App factory
# ---------------------------------------------------------------------------


def create_app() -> FastAPI:
    """Create the FastAPI relay application."""
    app = FastAPI(title="Repowire Relay", version="0.2.0")

    # -- Landing page --

    @app.get("/", response_class=HTMLResponse)
    async def landing() -> HTMLResponse:
        return HTMLResponse(_LANDING_HTML)

    # -- Health --

    @app.get("/health")
    async def health() -> dict[str, Any]:
        return {"status": "ok", "connected_daemons": len(_connections)}

    # -- Registration --

    class RegisterRequest(BaseModel):
        user_id: str

    @app.post("/api/v1/register")
    async def register(req: RegisterRequest) -> dict[str, str]:
        api_key = register_token(req.user_id)
        return {"api_key": api_key.key, "user_id": api_key.user_id}

    # -- Connected daemons (authenticated) --

    @app.get("/api/v1/daemons")
    async def list_daemons(api_key: APIKey = Depends(get_api_key)) -> list[dict[str, Any]]:
        daemons = _get_all_daemons(api_key.user_id)
        return [
            {
                "daemon_id": d.daemon_id,
                "connected_at": d.connected_at.isoformat(),
            }
            for d in daemons
        ]

    # -- WebSocket relay endpoint --

    @app.websocket("/ws/relay")
    async def ws_relay(ws: WebSocket) -> None:
        api_key_str = ws.query_params.get("api_key", "")
        daemon_id = ws.query_params.get("daemon_id", "")

        if not api_key_str or not daemon_id:
            await ws.close(code=4001, reason="Missing api_key or daemon_id")
            return

        api_key = validate_api_key(api_key_str)
        if not api_key:
            await ws.close(code=4003, reason="Invalid API key")
            return

        await ws.accept()

        conn = DaemonConnection(user_id=api_key.user_id, daemon_id=daemon_id, websocket=ws)

        # Evict stale connection for same daemon_id
        old = _get_daemon(api_key.user_id, daemon_id)
        if old:
            log.info("Evicting stale connection for %s/%s", api_key.user_id, daemon_id)
            _unregister(old)
            try:
                await old.websocket.close(code=4000, reason="Replaced by new connection")
            except Exception:
                pass

        _register(conn)

        try:
            while True:
                msg = await ws.receive_json()
                msg_type = msg.get("type", "")

                if msg_type == "pong":
                    continue

                if msg_type == "http_response":
                    await _handle_http_response(msg)
                    continue

                handler = _MSG_HANDLERS.get(msg_type)
                if handler:
                    await handler(conn, msg)
                else:
                    log.warning("Unknown message type %r from %s", msg_type, daemon_id)
        except WebSocketDisconnect:
            pass
        except Exception:
            log.exception("WebSocket error for %s/%s", api_key.user_id, daemon_id)
        finally:
            _unregister(conn)

    # -- HTTP tunnel --

    @app.api_route(
        "/d/{token}/{path:path}",
        methods=["GET", "POST", "PUT", "DELETE"],
    )
    async def http_tunnel(token: str, path: str, request: Request) -> Response:
        api_key = validate_api_key(token)
        if not api_key:
            raise HTTPException(status_code=401, detail="Invalid API key")

        conn = _get_any_daemon(api_key.user_id)
        if not conn:
            raise HTTPException(status_code=502, detail="No daemon connected")

        request_id = str(uuid4())

        # Collect headers (skip hop-by-hop)
        fwd_headers = {
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        }

        tunnel_msg: dict[str, Any] = {
            "type": "http_request",
            "request_id": request_id,
            "method": request.method,
            "path": f"/{path}",
            "headers": fwd_headers,
            "query_string": str(request.url.query) if request.url.query else "",
        }

        # Include body for non-GET
        if request.method != "GET":
            body_bytes = await request.body()
            if body_bytes:
                tunnel_msg["body"] = base64.b64encode(body_bytes).decode()

        future: asyncio.Future[dict[str, Any]] = asyncio.get_event_loop().create_future()
        _http_futures[request_id] = future

        try:
            await conn.websocket.send_json(tunnel_msg)
        except Exception:
            _http_futures.pop(request_id, None)
            raise HTTPException(status_code=502, detail="Failed to reach daemon")

        try:
            resp_msg = await asyncio.wait_for(future, timeout=HTTP_TUNNEL_TIMEOUT)
        except asyncio.TimeoutError:
            raise HTTPException(status_code=504, detail="Daemon did not respond in time")
        finally:
            _http_futures.pop(request_id, None)

        status = resp_msg.get("status", 200)
        resp_headers = resp_msg.get("headers", {})
        body_b64 = resp_msg.get("body", "")
        body = base64.b64decode(body_b64) if body_b64 else b""

        return Response(content=body, status_code=status, headers=resp_headers)

    return app
