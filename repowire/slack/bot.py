"""Slack bot peer for the repowire mesh.

Bridges Slack <> repowire: notifications become Slack messages,
Slack messages become peer notifications. Buttons for quick peer selection.

Usage:
    SLACK_BOT_TOKEN=xoxb-... SLACK_APP_TOKEN=xapp-... SLACK_CHANNEL_ID=C... repowire slack start
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from pathlib import Path
from typing import Any
from urllib.parse import urlparse, urlunparse

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.models import DEFAULT_DAEMON_URL

logger = logging.getLogger(__name__)


def _esc(text: str) -> str:
    """Escape Slack mrkdwn special characters."""
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def _ws_url(http_url: str) -> str:
    """Convert http(s) URL to ws(s)."""
    p = urlparse(http_url)
    return urlunparse(p._replace(scheme="wss" if p.scheme == "https" else "ws"))


class SlackPeer:
    """Slack bot that registers as a repowire peer via Socket Mode."""

    def __init__(
        self,
        bot_token: str,
        app_token: str,
        channel_id: str,
        daemon_url: str = DEFAULT_DAEMON_URL,
        display_name: str = "slack",
        circle: str = "default",
    ):
        self._bot_token = bot_token
        self._app_token = app_token
        self._channel_id = channel_id
        self._daemon_url = daemon_url.rstrip("/")
        self._display_name = display_name
        self._circle = circle
        self._http = httpx.AsyncClient(
            base_url="https://slack.com",
            headers={"Authorization": f"Bearer {bot_token}"},
            timeout=10.0,
        )
        self._daemon_http = httpx.AsyncClient(base_url=self._daemon_url, timeout=10.0)
        self._ws: ClientConnection | None = None
        self._slack_ws: Any = None
        self._stopping = False
        self._reply_target: str | None = None

    async def start(self) -> None:
        logger.info("Starting Slack peer")
        await asyncio.gather(self._daemon_ws_loop(), self._slack_ws_loop())

    async def stop(self) -> None:
        self._stopping = True
        if self._ws:
            try:
                await self._ws.close()
            except Exception:
                pass
        if self._slack_ws:
            try:
                await self._slack_ws.close()
            except Exception:
                pass
        await self._http.aclose()
        await self._daemon_http.aclose()

    # -- Daemon WebSocket --

    async def _daemon_ws_loop(self) -> None:
        url = f"{_ws_url(self._daemon_url)}/ws"
        backoff = 1.0
        while not self._stopping:
            try:
                async with websockets.connect(url) as ws:
                    self._ws = ws
                    backoff = 1.0
                    await ws.send(json.dumps({
                        "type": "connect",
                        "display_name": self._display_name,
                        "circle": self._circle,
                        "backend": "claude-code",
                        "path": "/slack",
                    }))

                    raw_resp = await ws.recv()
                    try:
                        resp = json.loads(raw_resp)
                        if not isinstance(resp, dict) or resp.get("type") != "connected":
                            logger.error("Daemon connect failed: %s", resp)
                            await asyncio.sleep(backoff)
                            continue
                    except json.JSONDecodeError:
                        logger.error("Daemon sent invalid JSON: %s", raw_resp)
                        await asyncio.sleep(backoff)
                        continue

                    logger.info("Daemon connected: %s", resp.get("session_id"))
                    async for raw in ws:
                        try:
                            await self._on_daemon_msg(json.loads(raw))
                        except Exception:
                            logger.exception("Error handling daemon message")
                            if self._stopping:
                                break
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("Daemon WS lost, retry in %.0fs", backoff, exc_info=True)
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _on_daemon_msg(self, msg: dict[str, Any]) -> None:
        if not isinstance(msg, dict):
            return
        t = msg.get("type", "")
        who = msg.get("from_peer", "?")
        text = msg.get("text", "")

        if t == "notify":
            await self._slack_send(f"*@{_esc(who)}*\n{_esc(text)}")
        elif t == "query":
            await self._slack_send(f":question: *@{_esc(who)}*\n{_esc(text)}")
        elif t == "broadcast":
            await self._slack_send(f":loudspeaker: *@{_esc(who)}*\n{_esc(text)}")
        elif t == "ping" and self._ws:
            await self._ws.send(json.dumps({"type": "pong"}))

    # -- Slack Socket Mode --

    async def _slack_ws_loop(self) -> None:
        """Connect to Slack via Socket Mode (apps.connections.open)."""
        backoff = 1.0
        while not self._stopping:
            try:
                wss_url = await self._get_socket_url()
                if not wss_url:
                    logger.error("Failed to get Slack Socket Mode URL")
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2, 30.0)
                    continue

                async with websockets.connect(wss_url) as ws:
                    self._slack_ws = ws
                    backoff = 1.0
                    logger.info("Slack Socket Mode connected")
                    async for raw in ws:
                        try:
                            data = json.loads(raw)
                            if await self._on_slack_event(ws, data) == "disconnect":
                                break
                        except Exception:
                            logger.exception("Error handling Slack event")
                            if self._stopping:
                                break
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("Slack WS lost, retry in %.0fs", backoff, exc_info=True)
                self._slack_ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _get_socket_url(self) -> str | None:
        """Call apps.connections.open to get a Socket Mode WSS URL."""
        try:
            r = await self._http.post(
                "/api/apps.connections.open",
                headers={"Authorization": f"Bearer {self._app_token}"},
            )
            r.raise_for_status()
            data = r.json()
            if data.get("ok"):
                return data["url"]
            logger.error("apps.connections.open failed: %s", data.get("error"))
        except Exception:
            logger.warning("Failed to get socket URL", exc_info=True)
        return None

    async def _on_slack_event(self, ws: Any, envelope: dict) -> str | None:
        """Handle a Socket Mode envelope. Returns 'disconnect' if Slack requested it."""
        if not isinstance(envelope, dict):
            return None

        envelope_id = envelope.get("envelope_id")
        payload = envelope.get("payload", {})
        event_type = envelope.get("type")

        # Acknowledge immediately
        if envelope_id:
            await ws.send(json.dumps({"envelope_id": envelope_id}))

        if event_type == "events_api":
            event = payload.get("event", {})
            await self._on_event(event)
        elif event_type == "interactive":
            await self._on_interaction(payload)
        elif event_type == "disconnect":
            logger.info("Slack requested disconnect, will reconnect")
            return "disconnect"
        return None

    async def _on_event(self, event: dict) -> None:
        """Handle Slack Events API event."""
        if event.get("type") != "message":
            return
        # Ignore bot messages and subtypes (edits, deletes, etc.)
        if event.get("bot_id") or event.get("subtype"):
            return
        if event.get("channel") != self._channel_id:
            return

        text = event.get("text", "").strip()
        if not text:
            return

        await self._on_text(text)

    async def _on_interaction(self, payload: dict) -> None:
        """Handle Slack interactive payload (button clicks)."""
        actions = payload.get("actions", [])
        if not actions:
            return
        action = actions[0]
        value = action.get("value", "")

        if value.startswith("target:"):
            peer = value.split(":", 1)[1]
            if peer:
                self._reply_target = peer
                await self._slack_send(f"Now talking to *@{_esc(peer)}*. All messages go there.")
        elif value == "cancel":
            self._reply_target = None
            await self._slack_send("Cancelled.")
        elif value == "peers":
            await self._cmd_peers()

    async def _on_text(self, text: str) -> None:
        """Route incoming Slack message text."""
        # Commands
        if text in ("peers", "list"):
            await self._cmd_peers()
            return
        if text == "clear":
            self._reply_target = None
            await self._slack_send("Cleared. No active conversation.")
            return

        if text.startswith(("switch ", "select ")):
            parts = text.split(maxsplit=1)
            if len(parts) < 2:
                await self._slack_send("Usage: `select <peer_name>`")
                return
            peer = parts[1].strip().lstrip("@")
            if peer:
                self._reply_target = peer
                await self._slack_send(f"Now talking to *@{_esc(peer)}*. All messages go there.")
            return

        # @peer message — explicit target (also sets sticky)
        m = re.match(r"^@(\S+)\s+(.+)", text, re.DOTALL)
        if m:
            peer = m.group(1)
            self._reply_target = peer
            await self._notify(peer, m.group(2))
            return

        # Sticky conversation
        if self._reply_target:
            await self._notify(self._reply_target, text)
            return

        # No conversation active
        await self._slack_send(
            "No active conversation.\n\n"
            "`peers` — list peers\n"
            "`select name` — start conversation\n"
            "`@name msg` — quick message"
        )

    # -- Commands --

    async def _cmd_peers(self) -> None:
        try:
            r = await self._daemon_http.get("/peers")
            r.raise_for_status()
            peers = r.json().get("peers", [])
            active = [p for p in peers if p.get("status") in ("online", "busy")]

            if not active:
                await self._slack_send("No peers online.")
                return

            blocks: list[dict] = []
            for p in active:
                name = p.get("display_name", p.get("name", "?"))
                path = p.get("path", "")
                folder = Path(path).name or name
                desc = p.get("description", "")
                branch = p.get("metadata", {}).get("branch", "")
                icon = ":large_green_circle:" if p.get("status") == "online" else ":yellow_circle:"

                folder_esc = _esc(folder)
                name_esc = _esc(name)
                line = f"{icon} *{folder_esc}* `{name_esc}`"
                if branch:
                    line += f" `{_esc(branch)}`"
                if desc:
                    line += f"\n_{_esc(desc)}_"

                blocks.append({
                    "type": "section",
                    "text": {"type": "mrkdwn", "text": line},
                    "accessory": {
                        "type": "button",
                        "text": {"type": "plain_text", "text": f"💬 {folder}"[:75]},
                        "value": f"target:{name}",
                        "action_id": f"select_{name}"[:255],
                    },
                })

            names = ", ".join(Path(p.get("path", "")).name or p.get("name", "?") for p in active)
            await self._slack_send(f"Online: {_esc(names)}", blocks)
        except Exception as e:
            logger.warning("Failed to list peers", exc_info=True)
            await self._slack_send(f"Error listing peers: {e}")

    async def _notify(self, peer: str, message: str) -> None:
        try:
            r = await self._daemon_http.post(
                "/notify",
                json={
                    "from_peer": self._display_name,
                    "to_peer": peer,
                    "text": message,
                    "bypass_circle": True,
                },
            )
            if r.status_code == 200:
                await self._slack_send(f":white_check_mark: → *@{_esc(peer)}*")
            else:
                try:
                    detail = r.json().get("detail", r.text)
                except Exception:
                    detail = r.text
                await self._slack_send(f":x: {_esc(detail)}")
        except Exception as e:
            logger.warning("Failed to notify peer %s", peer, exc_info=True)
            await self._slack_send(f"Error sending to {peer}: {e}")

    # -- Slack API --

    async def _slack_send(
        self, text: str, blocks: list[dict] | None = None
    ) -> None:
        try:
            payload: dict[str, Any] = {"channel": self._channel_id, "text": text}
            if blocks:
                payload["blocks"] = blocks
            r = await self._http.post("/api/chat.postMessage", json=payload)
            r.raise_for_status()
            data = r.json()
            if not data.get("ok"):
                logger.error("Slack postMessage failed: %s", data.get("error"))
        except Exception:
            logger.warning("Slack send failed", exc_info=True)


async def run_bot() -> None:
    """Run the bot in the current event loop."""
    bot_token = os.environ.get("SLACK_BOT_TOKEN")
    app_token = os.environ.get("SLACK_APP_TOKEN")
    channel = os.environ.get("SLACK_CHANNEL_ID")
    daemon = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

    if not bot_token or not app_token or not channel:
        print("Set SLACK_BOT_TOKEN, SLACK_APP_TOKEN, and SLACK_CHANNEL_ID env vars.")
        raise SystemExit(1)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    bot = SlackPeer(
        bot_token=bot_token,
        app_token=app_token,
        channel_id=channel,
        daemon_url=daemon,
    )
    try:
        await bot.start()
    finally:
        await bot.stop()


def main() -> None:
    """Entry point: repowire slack start"""
    try:
        asyncio.run(run_bot())
    except KeyboardInterrupt:
        pass
