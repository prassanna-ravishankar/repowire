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
from typing import Any

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.models import RelayConfig

logger = logging.getLogger(__name__)

_MAX_BACKOFF = 30.0
_INITIAL_BACKOFF = 1.0


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

    @property
    def connected(self) -> bool:
        """Whether currently connected to relay."""
        return self._ws is not None and self._ws.close_code is None

    async def start(self) -> None:
        """Start the relay client as a background task."""
        if not self._config.enabled or not self._config.api_key:
            logger.info("Relay disabled or no API key — skipping relay client")
            return
        self._stopping = False
        self._http = httpx.AsyncClient()
        self._task = asyncio.create_task(self._run_loop())
        logger.info("Relay client started (daemon_id=%s)", self._daemon_id)

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
                async with websockets.connect(self._build_url()) as ws:
                    self._ws = ws
                    backoff = _INITIAL_BACKOFF
                    logger.info("Connected to relay at %s", self._config.url)
                    await self._listen(ws)
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning(
                    "Relay connection lost, reconnecting in %.0fs", backoff, exc_info=True
                )
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, _MAX_BACKOFF)

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

    async def _handle_http_request(self, msg: dict[str, Any]) -> None:
        """Forward tunneled HTTP request to local daemon."""
        request_id = msg["request_id"]
        method = msg.get("method", "GET").upper()
        path = msg.get("path", "/")
        headers = msg.get("headers", {})
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
