"""Relay server for cross-machine Repowire daemon-to-daemon communication.

Provides:
- WebSocket bridge: daemons connect via /ws/relay, messages forwarded within user scope
- HTTP tunnel: authenticated browser sessions are proxied to a connected daemon via cookie
- Dashboard: serves Next.js static export directly, tunnels only API calls to daemon
- Landing page: minimal UI at / for entering an API key
"""

from __future__ import annotations

import asyncio
import base64
import logging
import os
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

from fastapi import (
    Cookie,
    Depends,
    FastAPI,
    Header,
    HTTPException,
    Request,
    WebSocket,
    WebSocketDisconnect,
)
from fastapi.responses import FileResponse, HTMLResponse, RedirectResponse, Response
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel

from repowire.relay.auth import APIKey, register_token, validate_api_key

log = logging.getLogger(__name__)


class RegisterRequest(BaseModel):
    user_id: str


# ---------------------------------------------------------------------------
# Data model
# ---------------------------------------------------------------------------

HTTP_TUNNEL_TIMEOUT = 30  # seconds

# Paths that the relay handles directly (not tunneled)
_RELAY_PATHS = frozenset({"/", "/health", "/auth", "/ws/relay", "/dashboard"})
_RELAY_PREFIXES = ("/api/v1/", "/d/", "/_next/")

# API paths tunneled to the daemon (everything else is static or relay-owned)
_TUNNEL_PREFIXES = (
    "/peers", "/events", "/query", "/notify", "/broadcast",
    "/session", "/response", "/spawn", "/ws",
)


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


async def _tunnel_request(
    conn: DaemonConnection, method: str, path: str, request: Request
) -> Response:
    """Tunnel an HTTP request to a daemon and return the response."""
    request_id = str(uuid4())

    fwd_headers = {
        k: v
        for k, v in request.headers.items()
        if k.lower() not in ("host", "connection", "transfer-encoding", "cookie")
    }

    tunnel_msg: dict[str, Any] = {
        "type": "http_request",
        "request_id": request_id,
        "method": method,
        "path": path,
        "headers": fwd_headers,
        "query_string": str(request.url.query) if request.url.query else "",
    }

    if method != "GET":
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
<title>Repowire — Mesh Network for AI Coding Agents</title>
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
  .container { text-align: center; max-width: 480px; padding: 2rem; }
  h1 { color: #e0e0e8; font-size: 1.8rem; margin-bottom: 0.4rem; }
  .tagline { color: #8a8a9a; font-size: 0.9rem; margin-bottom: 0.6rem; }
  .desc { color: #5a5a6a; font-size: 0.8rem; line-height: 1.5; margin-bottom: 2rem; }
  .divider { border: none; border-top: 1px solid #1a1a2a; margin: 1.5rem 0; }
  .access-label { color: #6a6a7a; font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.1em; margin-bottom: 0.6rem; }
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
  .error { color: #e05050; font-size: 0.8rem; margin-top: 0.6rem; display: none; }
  .setup { color: #4a4a5a; font-size: 0.75rem; margin-top: 1rem; }
  .setup code { color: #7a7a8a; background: #14141f; padding: 0.15rem 0.4rem;
    border-radius: 3px; }
  .links { margin-top: 2rem; display: flex; gap: 1.5rem; justify-content: center;
    font-size: 0.8rem; }
  .links a { color: #5a5a7a; text-decoration: none; }
  .links a:hover { color: #8a8aaa; }
</style>
</head>
<body>
<div class="container">
  <h1>repowire</h1>
  <p class="tagline">Mesh network for AI coding agents</p>
  <p class="desc">
    Let Claude Code and OpenCode sessions talk to each other across repos and machines.
  </p>
  <hr class="divider">
  <p class="access-label">Access your dashboard</p>
  <form action="/auth" method="POST">
    <input name="token" type="text" placeholder="rw_..." autocomplete="off" spellcheck="false">
    <button type="submit">Go</button>
  </form>
  <p id="err" class="error"></p>
  <p class="setup">Run <code>repowire setup --relay</code> to get your key</p>
  <div class="links">
    <a href="https://github.com/prassanna-ravishankar/repowire">GitHub</a>
    <a href="https://pypi.org/project/repowire/">PyPI</a>
    <a href="https://prassanna.io/blog/repowire/">Blog</a>
  </div>
</div>
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


def _find_web_output_dir() -> str | None:
    """Find the web/out directory for dashboard static files."""
    import sys

    # Check relative to this file (works in Docker where web/out is at /app/web/out)
    relay_dir = os.path.dirname(os.path.abspath(__file__))
    repo_root = os.path.dirname(os.path.dirname(relay_dir))
    dev_web_out = os.path.join(repo_root, "web", "out")
    if os.path.isfile(os.path.join(dev_web_out, "dashboard.html")):
        return dev_web_out

    # Installed mode: web/out is sibling to repowire package in site-packages
    for path in sys.path:
        installed = os.path.join(path, "web", "out")
        if os.path.isfile(os.path.join(installed, "dashboard.html")):
            return installed

    return None


def create_app() -> FastAPI:
    """Create the FastAPI relay application."""
    app = FastAPI(title="Repowire Relay", version="0.3.0")

    # -- Dashboard static files --
    web_out = _find_web_output_dir()
    if web_out:
        next_static = os.path.join(web_out, "_next")
        if os.path.exists(next_static):
            app.mount("/_next", StaticFiles(directory=next_static), name="next_static")
        log.info("Serving dashboard from %s", web_out)
    else:
        log.warning("web/out not found — dashboard will not be available")

    # -- Landing page --

    @app.get("/", response_class=HTMLResponse)
    async def landing(rw_token: str | None = Cookie(default=None)) -> Response:
        # If user already has a valid cookie, redirect to dashboard
        if rw_token and validate_api_key(rw_token):
            return RedirectResponse(url="/dashboard", status_code=302)
        return HTMLResponse(_LANDING_HTML)

    # -- Auth (sets cookie, redirects to dashboard) --

    @app.post("/auth")
    async def auth(request: Request) -> Response:
        form = await request.form()
        token = str(form.get("token", "")).strip()
        if not token:
            raise HTTPException(status_code=400, detail="Missing token")

        api_key = validate_api_key(token)
        if not api_key:
            raise HTTPException(status_code=401, detail="Invalid API key")

        conn = _get_any_daemon(api_key.user_id)
        if not conn:
            raise HTTPException(status_code=502, detail="No daemon connected")

        response = RedirectResponse(url="/dashboard", status_code=303)
        response.set_cookie(
            key="rw_token",
            value=token,
            httponly=True,
            secure=True,
            samesite="lax",
            max_age=30 * 24 * 3600,  # 30 days
        )
        return response

    # -- Dashboard (served from static files, auth-gated) --

    @app.get("/dashboard", response_class=HTMLResponse)
    async def dashboard(rw_token: str | None = Cookie(default=None)) -> Response:
        if not rw_token or not validate_api_key(rw_token):
            return RedirectResponse(url="/", status_code=302)
        if not web_out:
            return HTMLResponse("Dashboard not built. Rebuild relay image.", status_code=503)
        dashboard_path = os.path.join(web_out, "dashboard.html")
        if not os.path.exists(dashboard_path):
            return HTMLResponse("dashboard.html not found", status_code=404)
        return FileResponse(dashboard_path)

    # -- Health --

    @app.get("/health")
    async def health() -> dict[str, Any]:
        return {"status": "ok", "connected_daemons": len(_connections)}

    # -- Registration --

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

    # -- Legacy /d/{token}/ tunnel (still supported) --

    @app.api_route(
        "/d/{token}/{path:path}",
        methods=["GET", "POST", "PUT", "DELETE"],
    )
    async def legacy_tunnel(token: str, path: str, request: Request) -> Response:
        api_key = validate_api_key(token)
        if not api_key:
            raise HTTPException(status_code=401, detail="Invalid API key")
        conn = _get_any_daemon(api_key.user_id)
        if not conn:
            raise HTTPException(status_code=502, detail="No daemon connected")
        return await _tunnel_request(conn, request.method, f"/{path}", request)

    # -- Cookie-based tunnel (only API paths are proxied to daemon) --

    @app.api_route(
        "/{path:path}",
        methods=["GET", "POST", "PUT", "DELETE"],
    )
    async def cookie_tunnel(
        path: str, request: Request, rw_token: str | None = Cookie(default=None)
    ) -> Response:
        full_path = f"/{path}"

        # Only tunnel daemon API paths
        if not any(full_path.startswith(p) for p in _TUNNEL_PREFIXES):
            raise HTTPException(status_code=404)

        if not rw_token:
            return RedirectResponse(url="/", status_code=302)

        api_key = validate_api_key(rw_token)
        if not api_key:
            return RedirectResponse(url="/", status_code=302)

        conn = _get_any_daemon(api_key.user_id)
        if not conn:
            raise HTTPException(status_code=502, detail="No daemon connected")

        return await _tunnel_request(conn, request.method, full_path, request)

    return app
