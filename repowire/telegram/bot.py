"""Telegram bot that acts as a repowire peer.

Bridges Telegram <> repowire mesh:
- Telegram messages -> notify/ask peers
- Peer notifications -> Telegram messages
- Inline buttons for quick actions

Usage:
    TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from typing import Any
from urllib.parse import urlparse, urlunparse

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.models import DEFAULT_DAEMON_URL

logger = logging.getLogger(__name__)

# Callback data prefixes
CB_NOTIFY = "notify:"
CB_PEER_NOTIFY = "peer_notify:"

# Message routing: @peername message
PEER_MSG_RE = re.compile(r"^@(\S+)\s+(.+)", re.DOTALL)

# Pre-compiled MarkdownV2 escape regex
_MD_ESCAPE_RE = re.compile(r"([_*\[\]()~`>#+=|{}.!\-])")


def _escape_md(text: str) -> str:
    """Escape special characters for Telegram MarkdownV2."""
    return _MD_ESCAPE_RE.sub(r"\\\1", text)


def _inline_kb(buttons: list[list[dict]]) -> dict:
    """Build Telegram InlineKeyboardMarkup."""
    return {"inline_keyboard": buttons}


def _http_to_ws(url: str) -> str:
    """Convert http(s) URL to ws(s) URL."""
    parsed = urlparse(url)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return urlunparse(parsed._replace(scheme=scheme))


class TelegramPeer:
    """Telegram bot that registers as a repowire peer."""

    def __init__(
        self,
        bot_token: str,
        chat_id: str,
        daemon_url: str = DEFAULT_DAEMON_URL,
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
        self._tg_offset = 0
        self._tg_api = f"https://api.telegram.org/bot{bot_token}"

    async def start(self) -> None:
        """Start the bot — connects to daemon and polls Telegram."""
        logger.info("Starting Telegram peer: %s", self._display_name)
        await asyncio.gather(self._ws_loop(), self._telegram_poll_loop())

    async def stop(self) -> None:
        self._stopping = True
        if self._ws:
            await self._ws.close()
        await self._http.aclose()

    # -- Daemon WebSocket --

    async def _ws_loop(self) -> None:
        """Maintain WebSocket connection to daemon."""
        ws_url = f"{_http_to_ws(self._daemon_url)}/ws"
        backoff = 1.0

        while not self._stopping:
            try:
                async with websockets.connect(ws_url) as ws:
                    self._ws = ws
                    backoff = 1.0
                    await ws.send(json.dumps({
                        "type": "connect",
                        "display_name": self._display_name,
                        "circle": self._circle,
                        "backend": "claude-code",
                        "path": "/telegram",
                    }))
                    resp = json.loads(await ws.recv())
                    if resp.get("type") != "connected":
                        logger.error("Connect failed: %s", resp)
                        await asyncio.sleep(backoff)
                        continue
                    logger.info("Connected as %s", resp.get("session_id"))
                    async for raw in ws:
                        await self._handle_ws_message(json.loads(raw))
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("WS lost, retry in %.0fs", backoff, exc_info=True)
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _handle_ws_message(self, msg: dict[str, Any]) -> None:
        msg_type = msg.get("type", "")
        from_peer = msg.get("from_peer", "?")
        text = msg.get("text", "")

        if msg_type == "notify":
            await self._send_telegram(
                f"*@{_escape_md(from_peer)}*\n{_escape_md(text)}",
                reply_markup=_inline_kb([[
                    {"text": "Reply", "callback_data": f"{CB_NOTIFY}{from_peer}"},
                ]]),
            )
        elif msg_type == "query":
            await self._send_telegram(
                f"*@{_escape_md(from_peer)}* \\(query\\)\n{_escape_md(text)}",
                reply_markup=_inline_kb([[
                    {"text": "Reply", "callback_data": f"{CB_NOTIFY}{from_peer}"},
                ]]),
            )
        elif msg_type == "broadcast":
            await self._send_telegram(
                f"*@{_escape_md(from_peer)}* \\(broadcast\\)\n"
                f"{_escape_md(text)}"
            )
        elif msg_type == "ping" and self._ws:
            await self._ws.send(json.dumps({"type": "pong"}))

    # -- Telegram polling --

    async def _telegram_poll_loop(self) -> None:
        while not self._stopping:
            try:
                resp = await self._http.get(
                    f"{self._tg_api}/getUpdates",
                    params={"offset": self._tg_offset, "timeout": 30},
                    timeout=35,
                )
                for update in resp.json().get("result", []):
                    self._tg_offset = update["update_id"] + 1
                    await self._handle_update(update)
            except asyncio.CancelledError:
                break
            except Exception:
                logger.warning("Telegram poll error", exc_info=True)
                await asyncio.sleep(5)

    async def _handle_update(self, update: dict) -> None:
        """Handle a Telegram update (message or callback)."""
        cb = update.get("callback_query")
        if cb:
            chat_id = str(cb.get("message", {}).get("chat", {}).get("id", ""))
            if chat_id == self._chat_id:
                await self._handle_callback(cb)
            return

        message = update.get("message", {})
        text = message.get("text", "")
        chat_id = str(message.get("chat", {}).get("id", ""))
        if chat_id == self._chat_id and text:
            await self._handle_telegram_message(text.strip())

    async def _handle_callback(self, cb: dict) -> None:
        """Handle inline button press."""
        data = cb.get("data", "")

        await self._http.post(
            f"{self._tg_api}/answerCallbackQuery",
            json={"callback_query_id": cb.get("id", "")},
        )

        if data.startswith(CB_NOTIFY):
            peer_name = data.removeprefix(CB_NOTIFY)
            await self._send_telegram(
                f"_Type_ `@{_escape_md(peer_name)} your message`"
            )
        elif data.startswith(CB_PEER_NOTIFY):
            peer_name = data.removeprefix(CB_PEER_NOTIFY)
            await self._send_telegram(
                f"_Type_ `@{_escape_md(peer_name)} your message`"
            )

    async def _handle_telegram_message(self, text: str) -> None:
        if text in ("/peers", "/start"):
            await self._cmd_peers()
        elif match := PEER_MSG_RE.match(text):
            await self._cmd_notify(match.group(1), match.group(2))
        else:
            await self._send_telegram(
                "`/peers` — list online peers\n"
                "`@name msg` — notify a peer"
            )

    # -- Commands --

    async def _cmd_peers(self) -> None:
        try:
            resp = await self._http.get(f"{self._daemon_url}/peers")
            data = resp.json()
            peers = data.get("peers", data) if isinstance(data, dict) else data
            active = [p for p in peers if p.get("status") in ("online", "busy")]

            if not active:
                await self._send_telegram("No peers online\\.")
                return

            lines = []
            buttons = []
            for p in active:
                name = p.get("display_name", p.get("name", "?"))
                status = p.get("status", "?")
                desc = p.get("description", "")
                path = p.get("path", "")
                folder = path.rstrip("/").split("/")[-1] if path else ""
                branch = p.get("metadata", {}).get("branch", "")
                icon = "🟢" if status == "online" else "🟡"

                line = f"{icon} `{_escape_md(name)}` — {_escape_md(folder)}"
                if branch:
                    line += f" \\(`{_escape_md(branch)}`\\)"
                if desc:
                    line += f"\n    _{_escape_md(desc)}_"
                lines.append(line)
                buttons.append([{
                    "text": f"📨 {folder or name}",
                    "callback_data": f"{CB_PEER_NOTIFY}{name}",
                }])

            await self._send_telegram(
                "\n".join(lines),
                reply_markup=_inline_kb(buttons),
            )
        except Exception as e:
            await self._send_telegram(f"Error: {_escape_md(str(e))}")

    async def _cmd_notify(self, peer_name: str, message: str) -> None:
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

    # -- Telegram API --

    async def _send_telegram(
        self, text: str, reply_markup: dict | None = None,
    ) -> None:
        try:
            payload: dict[str, Any] = {
                "chat_id": self._chat_id,
                "text": text,
                "parse_mode": "MarkdownV2",
            }
            if reply_markup:
                payload["reply_markup"] = reply_markup
            await self._http.post(f"{self._tg_api}/sendMessage", json=payload)
        except Exception:
            logger.warning("Failed to send Telegram message", exc_info=True)


def main() -> None:
    """Entry point for `repowire telegram start`."""
    bot_token = os.environ.get("TELEGRAM_BOT_TOKEN")
    chat_id = os.environ.get("TELEGRAM_CHAT_ID")
    daemon_url = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

    if not bot_token or not chat_id:
        print("Set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID env vars.")
        print("Get token from @BotFather, chat ID from @userinfobot.")
        raise SystemExit(1)

    logging.basicConfig(
        level=logging.INFO, format="%(levelname)s %(name)s: %(message)s",
    )
    bot = TelegramPeer(bot_token=bot_token, chat_id=chat_id, daemon_url=daemon_url)
    try:
        asyncio.run(bot.start())
    except KeyboardInterrupt:
        asyncio.run(bot.stop())
