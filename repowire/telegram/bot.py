"""Telegram bot that acts as a repowire peer.

Bridges Telegram ↔ repowire mesh:
- Telegram messages → notify/ask peers
- Peer notifications → Telegram messages

Usage:
    TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
    # or with relay:
    REPOWIRE_DAEMON_URL=http://127.0.0.1:8377 repowire telegram start
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from typing import Any

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

logger = logging.getLogger(__name__)

# Telegram API base
TG_API = "https://api.telegram.org/bot{token}"

# Message routing pattern: @peername message
PEER_MSG_RE = re.compile(r"^@(\S+)\s+(.+)", re.DOTALL)


class TelegramPeer:
    """Telegram bot that registers as a repowire peer."""

    def __init__(
        self,
        bot_token: str,
        chat_id: str,
        daemon_url: str = "http://127.0.0.1:8377",
        display_name: str = "telegram",
        circle: str = "default",
    ):
        self._token = bot_token
        self._chat_id = chat_id
        self._daemon_url = daemon_url.rstrip("/")
        self._display_name = display_name
        self._circle = circle
        self._http = httpx.AsyncClient()
        self._ws: ClientConnection | None = None
        self._stopping = False
        self._tg_offset = 0  # Telegram update offset

    async def start(self) -> None:
        """Start the bot — connects to daemon and polls Telegram."""
        logger.info("Starting Telegram peer: %s", self._display_name)

        # Run both loops concurrently
        await asyncio.gather(
            self._ws_loop(),
            self._telegram_poll_loop(),
        )

    async def stop(self) -> None:
        """Graceful shutdown."""
        self._stopping = True
        if self._ws:
            await self._ws.close()
        await self._http.aclose()

    # -- Daemon WebSocket (receive notifications from peers) --

    async def _ws_loop(self) -> None:
        """Maintain WebSocket connection to daemon, forward messages to Telegram."""
        ws_url = self._daemon_url.replace("http://", "ws://").replace("https://", "wss://")
        ws_url = f"{ws_url}/ws"

        backoff = 1.0
        while not self._stopping:
            try:
                async with websockets.connect(ws_url) as ws:
                    self._ws = ws
                    backoff = 1.0

                    # Register as a peer
                    await ws.send(json.dumps({
                        "type": "connect",
                        "display_name": self._display_name,
                        "circle": self._circle,
                        "backend": "claude-code",  # treated identically by daemon
                        "path": "/telegram",
                    }))

                    resp = json.loads(await ws.recv())
                    if resp.get("type") != "connected":
                        logger.error("Failed to connect: %s", resp)
                        await asyncio.sleep(backoff)
                        continue

                    logger.info("Connected to daemon as %s (session: %s)",
                                self._display_name, resp.get("session_id"))

                    # Listen for messages
                    async for raw in ws:
                        msg = json.loads(raw)
                        await self._handle_ws_message(msg)

            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("WS connection lost, reconnecting in %.0fs", backoff, exc_info=True)
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _handle_ws_message(self, msg: dict[str, Any]) -> None:
        """Handle incoming WebSocket message from daemon."""
        msg_type = msg.get("type", "")

        if msg_type == "notify":
            from_peer = msg.get("from_peer", "?")
            text = msg.get("text", "")
            await self._send_telegram(f"*@{_escape_md(from_peer)}*\n{_escape_md(text)}")

        elif msg_type == "query":
            from_peer = msg.get("from_peer", "?")
            text = msg.get("text", "")
            cid = msg.get("correlation_id", "")
            await self._send_telegram(
                f"*@{_escape_md(from_peer)}* \\(query\\)\n"
                f"{_escape_md(text)}\n\n"
                f"_Reply with_ `/reply {_escape_md(cid[:8])} your answer`"
            )

        elif msg_type == "broadcast":
            from_peer = msg.get("from_peer", "?")
            text = msg.get("text", "")
            await self._send_telegram(
                f"*@{_escape_md(from_peer)}* \\(broadcast\\)\n{_escape_md(text)}"
            )

        elif msg_type == "ping":
            if self._ws:
                await self._ws.send(json.dumps({"type": "pong"}))

    # -- Telegram polling (receive messages from user) --

    async def _telegram_poll_loop(self) -> None:
        """Long-poll Telegram for user messages."""
        while not self._stopping:
            try:
                updates = await self._get_telegram_updates()
                for update in updates:
                    self._tg_offset = update["update_id"] + 1
                    message = update.get("message", {})
                    text = message.get("text", "")
                    chat_id = str(message.get("chat", {}).get("id", ""))

                    # Only process messages from the authorized chat
                    if chat_id != self._chat_id:
                        continue

                    if text:
                        await self._handle_telegram_message(text)

            except asyncio.CancelledError:
                break
            except Exception:
                logger.warning("Telegram poll error", exc_info=True)
                await asyncio.sleep(5)

    async def _handle_telegram_message(self, text: str) -> None:
        """Route a Telegram message to the mesh."""
        text = text.strip()

        # /peers — list online peers
        if text == "/peers":
            await self._cmd_peers()
            return

        # /reply <cid> <text> — respond to a query
        if text.startswith("/reply "):
            parts = text.split(maxsplit=2)
            if len(parts) >= 3:
                await self._cmd_reply(parts[1], parts[2])
            return

        # @peername message — notify a peer
        match = PEER_MSG_RE.match(text)
        if match:
            peer_name = match.group(1)
            message = match.group(2)
            await self._cmd_notify(peer_name, message)
            return

        # Default: show help
        await self._send_telegram(
            "Commands:\n"
            "`/peers` — list online peers\n"
            "`@peername message` — send notification to peer\n"
            "`/reply <id> answer` — reply to a query"
        )

    async def _cmd_peers(self) -> None:
        """List peers and send to Telegram."""
        try:
            resp = await self._http.get(f"{self._daemon_url}/peers")
            data = resp.json()
            peers = data.get("peers", data) if isinstance(data, dict) else data

            active = [p for p in peers if p.get("status") in ("online", "busy")]
            if not active:
                await self._send_telegram("No peers online\\.")
                return

            lines = []
            for p in active:
                name = p.get("display_name", p.get("name", "?"))
                status = p.get("status", "?")
                desc = p.get("description", "")
                path = p.get("path", "")
                folder = path.rstrip("/").split("/")[-1] if path else ""
                icon = "🟢" if status == "online" else "🟡"
                line = f"{icon} `{name}` — {folder}"
                if desc:
                    line += f" _{_escape_md(desc)}_"
                lines.append(line)

            await self._send_telegram("\n".join(lines))

        except Exception as e:
            await self._send_telegram(f"Error: {_escape_md(str(e))}")

    async def _cmd_notify(self, peer_name: str, message: str) -> None:
        """Send a notification to a peer."""
        try:
            resp = await self._http.post(
                f"{self._daemon_url}/notify",
                json={
                    "from_peer": self._display_name,
                    "to_peer": peer_name,
                    "text": message,
                    "bypass_circle": True,
                },
            )
            if resp.status_code == 200:
                await self._send_telegram(f"✓ Sent to @{_escape_md(peer_name)}")
            else:
                detail = resp.json().get("detail", resp.text)
                await self._send_telegram(f"✗ {_escape_md(str(detail))}")
        except Exception as e:
            await self._send_telegram(f"Error: {_escape_md(str(e))}")

    async def _cmd_reply(self, cid_prefix: str, text: str) -> None:
        """Reply to a pending query (not yet implemented — needs correlation_id routing)."""
        await self._send_telegram(
            "Query replies not yet supported\\. Use `@peername` to notify instead\\."
        )

    # -- Telegram API helpers --

    async def _get_telegram_updates(self) -> list[dict]:
        """Long-poll Telegram for updates."""
        resp = await self._http.get(
            f"{TG_API.format(token=self._token)}/getUpdates",
            params={"offset": self._tg_offset, "timeout": 30},
            timeout=35,
        )
        data = resp.json()
        return data.get("result", [])

    async def _send_telegram(self, text: str) -> None:
        """Send a message to the authorized Telegram chat."""
        try:
            await self._http.post(
                f"{TG_API.format(token=self._token)}/sendMessage",
                json={
                    "chat_id": self._chat_id,
                    "text": text,
                    "parse_mode": "MarkdownV2",
                },
            )
        except Exception:
            logger.warning("Failed to send Telegram message", exc_info=True)


def _escape_md(text: str) -> str:
    """Escape special characters for Telegram MarkdownV2."""
    special = r"_*[]()~`>#+-=|{}.!"
    return re.sub(f"([{re.escape(special)}])", r"\\\1", text)


def main() -> None:
    """Entry point for `repowire telegram start`."""
    bot_token = os.environ.get("TELEGRAM_BOT_TOKEN")
    chat_id = os.environ.get("TELEGRAM_CHAT_ID")
    daemon_url = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")

    if not bot_token or not chat_id:
        print("Required environment variables:")
        print("  TELEGRAM_BOT_TOKEN — from @BotFather")
        print("  TELEGRAM_CHAT_ID   — your chat ID (use @userinfobot to find)")
        print("  REPOWIRE_DAEMON_URL — daemon URL (default: http://127.0.0.1:8377)")
        raise SystemExit(1)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")

    bot = TelegramPeer(
        bot_token=bot_token,
        chat_id=chat_id,
        daemon_url=daemon_url,
    )

    try:
        asyncio.run(bot.start())
    except KeyboardInterrupt:
        pass
