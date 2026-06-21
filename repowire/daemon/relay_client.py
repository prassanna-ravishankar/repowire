"""Relay client — outbound WebSocket connector to the hosted relay server.

When relay is enabled, the local daemon maintains a persistent connection
to the relay, enabling cross-machine mesh messaging and HTTP tunneling.
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import socket
from datetime import datetime, timezone
from typing import Any

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.relay import RelayConfig

logger = logging.getLogger(__name__)

_MAX_BACKOFF = 30.0
_INITIAL_BACKOFF = 1.0

# Application-level keepalive. The relay (or an intermediary proxy) silently
# drops idle connections; without an explicit ping the daemon can sit in
# ``async for raw in ws`` on a half-open socket that never yields. websockets
# raises ConnectionClosed from the keepalive task when a pong is missed, which
# breaks the listen loop and triggers a reconnect. Tuned tighter than the
# library defaults so a dead connection is caught within ~30s, not minutes.
_PING_INTERVAL = 20.0
_PING_TIMEOUT = 20.0


class RelayClient:
    """Client that connects the local daemon to the hosted relay server."""

    def __init__(
        self,
        config: RelayConfig,
        daemon_id: str | None = None,
        local_base_url: str = "http://127.0.0.1:8377",
    ):
        self._config = config
        self._daemon_id = daemon_id or socket.gethostname()
        self._local_base_url = local_base_url
        self._ws: ClientConnection | None = None
        self._task: asyncio.Task[None] | None = None
        self._http: httpx.AsyncClient | None = None
        self._stopping = False
        # Liveness telemetry surfaced via /health so a dead relay client is
        # visible instead of masked behind the static relay-enabled config flag.
        self._last_connected_at: datetime | None = None
        self._last_error: str | None = None
        self._last_error_at: datetime | None = None

    @property
    def connected(self) -> bool:
        """Whether currently connected to relay."""
        return self._ws is not None and self._ws.close_code is None

    def status(self) -> dict[str, Any]:
        """Live connection telemetry for health reporting (not the config flag)."""
        task = self._task
        return {
            "connected": self.connected,
            "running": task is not None and not task.done(),
            "url": self._config.url,
            "daemon_id": self._daemon_id,
            "last_connected_at": (
                self._last_connected_at.isoformat()
                if self._last_connected_at
                else None
            ),
            "last_error": self._last_error,
            "last_error_at": (
                self._last_error_at.isoformat() if self._last_error_at else None
            ),
        }

    async def start(self) -> None:
        """Start the relay client as a background task.

        Idempotent: if a prior loop task is still alive this is a no-op, so it
        doubles as a lazy self-heal — a caller that observes a dead task (via
        :meth:`ensure_running`) can restart without spawning duplicates.
        """
        if not self._config.enabled or not self._config.api_key:
            logger.info("Relay disabled or no API key — skipping relay client")
            return
        if self._task is not None and not self._task.done():
            return
        self._stopping = False
        if self._http is None:
            self._http = httpx.AsyncClient()
        self._task = asyncio.create_task(self._run_loop())
        logger.info("Relay client started (daemon_id=%s)", self._daemon_id)

    async def ensure_running(self) -> bool:
        """Lazy self-heal: restart the loop if it died. Returns True if it relaunched.

        Lazy-repair, not polling: call this from request paths (e.g. /health),
        never on a timer. The hardened :meth:`_run_loop` should never exit on
        its own, so this is a belt-and-braces backstop, not the primary
        mechanism.
        """
        if self._stopping or not self._config.enabled or not self._config.api_key:
            return False
        if self._task is not None and not self._task.done():
            return False
        logger.warning("Relay loop was not running; relaunching (lazy self-heal)")
        await self.start()
        return True

    async def stop(self) -> None:
        """Gracefully disconnect."""
        self._stopping = True
        if self._http:
            await self._http.aclose()
            self._http = None
        if self._ws:
            await self._ws.close()
            self._ws = None
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None
        logger.info("Relay client stopped")

    async def send_message(self, message: dict[str, Any]) -> None:
        """Send a message to the relay."""
        if not self.connected or self._ws is None:
            raise ConnectionError("Not connected to relay")
        await self._ws.send(json.dumps(message))

    # -- Outbound helpers for cross-machine routing --

    async def relay_query(self, target_daemon_id: str, payload: dict[str, Any]) -> None:
        """Send a query to a peer on another daemon via relay."""
        await self.send_message(
            {
                "type": "relay_query",
                "target_daemon_id": target_daemon_id,
                "payload": payload,
            }
        )

    async def relay_notify(self, target_daemon_id: str, payload: dict[str, Any]) -> None:
        """Send a notification to a peer on another daemon via relay."""
        await self.send_message(
            {
                "type": "relay_notify",
                "target_daemon_id": target_daemon_id,
                "payload": payload,
            }
        )

    async def relay_broadcast(self, payload: dict[str, Any]) -> None:
        """Broadcast to all daemons connected to relay."""
        await self.send_message(
            {
                "type": "relay_broadcast",
                "payload": payload,
            }
        )

    # -- Internal --

    def _build_url(self) -> str:
        url = self._config.url.rstrip("/")
        return f"{url}/ws/relay?api_key={self._config.api_key}&daemon_id={self._daemon_id}"

    async def _run_loop(self) -> None:
        """Reconnect loop with exponential backoff."""
        backoff = _INITIAL_BACKOFF
        while not self._stopping:
            try:
                async with websockets.connect(
                    self._build_url(),
                    open_timeout=10,
                    close_timeout=5,
                    ping_interval=_PING_INTERVAL,
                    ping_timeout=_PING_TIMEOUT,
                    max_size=16 * 1024 * 1024,  # 16MB for attachment tunneling
                ) as ws:
                    self._ws = ws
                    backoff = _INITIAL_BACKOFF
                    self._last_connected_at = datetime.now(timezone.utc)
                    logger.info("Connected to relay at %s", self._config.url)
                    # Recreate httpx client on each reconnect (clear stale state)
                    if self._http:
                        await self._http.aclose()
                    self._http = httpx.AsyncClient()
                    await self._listen(ws)
            except asyncio.CancelledError:
                break
            except Exception as exc:
                # Record the error before the stopping-check: a failure is a
                # failure even if a shutdown is concurrently in flight, and
                # surfacing it in /health is the whole point.
                self._ws = None
                self._last_error = f"{type(exc).__name__}: {exc}"
                self._last_error_at = datetime.now(timezone.utc)
                if self._stopping:
                    break
                logger.warning(
                    "Relay connection lost, reconnecting in %.0fs", backoff, exc_info=True
                )
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, _MAX_BACKOFF)
            else:
                # _listen returned without an exception (server closed the
                # stream cleanly, e.g. 1000/1001). Reconnect rather than
                # exiting — falling out of the while body silently here is how
                # the client could go permanently dark — but back off and log
                # like the error path so a relay that repeatedly accepts and
                # immediately closes cannot spin a tight reconnect loop.
                self._ws = None
                if self._stopping:
                    break
                logger.info(
                    "Relay closed the connection cleanly, reconnecting in %.0fs",
                    backoff,
                )
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, _MAX_BACKOFF)
        # Loop exits only via stop()/cancel. A done task while not stopping is
        # treated as dead by ensure_running() (lazy self-heal on next /health).
        logger.info("Relay reconnect loop exited (stopping=%s)", self._stopping)

    async def _listen(self, ws: ClientConnection) -> None:
        """Listen for messages from relay and dispatch."""
        async for raw in ws:
            try:
                msg = json.loads(raw)
                await self._handle_message(msg)
            except Exception:
                logger.exception("Error handling relay message")

    async def _handle_message(self, msg: dict[str, Any]) -> None:
        msg_type = msg.get("type", "")

        if msg_type == "ping":
            await self.send_message({"type": "pong"})
            return

        if msg_type in ("relay_query", "relay_notify", "relay_broadcast"):
            await self._handle_relay_message(msg_type, msg)
            return

        if msg_type == "http_request":
            await self._handle_http_request(msg)
            return

        logger.debug("Unknown relay message type: %s", msg_type)

    async def _handle_relay_message(self, msg_type: str, msg: dict[str, Any]) -> None:
        """Forward relay_query/notify/broadcast to local daemon HTTP API."""
        endpoint_map = {
            "relay_query": "/query",
            "relay_notify": "/notify",
            "relay_broadcast": "/broadcast",
        }
        endpoint = endpoint_map[msg_type]
        payload = msg.get("payload", {})

        assert self._http is not None
        resp = await self._http.post(f"{self._local_base_url}{endpoint}", json=payload)

        response_body = (
            resp.json()
            if resp.headers.get("content-type", "").startswith("application/json")
            else {"text": resp.text}
        )

        await self.send_message(
            {
                "type": "relay_response",
                "correlation_id": msg.get("correlation_id"),
                "source_daemon_id": msg.get("source_daemon_id"),
                "status": resp.status_code,
                "body": response_body,
            }
        )

    # Headers that must not be forwarded to the local daemon — they cause
    # uvicorn's ProxyHeadersMiddleware to override request.client with the
    # browser's IP, breaking require_localhost checks.
    _STRIP_HEADERS = frozenset({
        "x-forwarded-for", "x-forwarded-proto", "x-forwarded-host",
        "x-real-ip", "forwarded",
    })

    async def _handle_http_request(self, msg: dict[str, Any]) -> None:
        """Forward tunneled HTTP request to local daemon."""
        request_id = msg["request_id"]
        method = msg.get("method", "GET").upper()
        path = msg.get("path", "/")
        headers = {
            k: v for k, v in msg.get("headers", {}).items()
            if k.lower() not in self._STRIP_HEADERS
        }
        query_string = msg.get("query_string", "")
        body_b64 = msg.get("body")

        url = f"{self._local_base_url}{path}"
        if query_string:
            url = f"{url}?{query_string}"

        content = base64.b64decode(body_b64) if body_b64 else None

        assert self._http is not None
        resp = await self._http.request(method, url, headers=headers, content=content)

        resp_headers = dict(resp.headers)
        resp_body_b64 = base64.b64encode(resp.content).decode("ascii")

        await self.send_message(
            {
                "type": "http_response",
                "request_id": request_id,
                "status": resp.status_code,
                "headers": resp_headers,
                "body": resp_body_b64,
            }
        )
