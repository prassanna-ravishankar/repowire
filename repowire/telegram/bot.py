"""Telegram bot peer for the repowire mesh.

Bridges Telegram <> repowire: notifications become Telegram messages,
Telegram messages become peer notifications. Inline buttons for quick actions.

Usage:
    TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
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

_MD_ESCAPE_RE = re.compile(r"([_*\[\]()~`>#+=|{}.!\-])")


def _esc(text: str) -> str:
    """Escape for Telegram MarkdownV2."""
    return _MD_ESCAPE_RE.sub(r"\\\1", text)


def _kb(rows: list[list[tuple[str, str]]]) -> dict:
    """Build InlineKeyboardMarkup from [(text, callback_data), ...] rows."""
    return {"inline_keyboard": [
        [{"text": t, "callback_data": d} for t, d in row] for row in rows
    ]}


def _ws_url(http_url: str) -> str:
    """Convert http(s) URL to ws(s)."""
    p = urlparse(http_url)
    return urlunparse(p._replace(scheme="wss" if p.scheme == "https" else "ws"))


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
        self._chat_id = chat_id
        self._daemon_url = daemon_url.rstrip("/")
        self._display_name = display_name
        self._circle = circle
        self._bot_path = f"/bot{bot_token}"
        self._http = httpx.AsyncClient(base_url="https://api.telegram.org", timeout=10.0)
        self._ws: ClientConnection | None = None
        self._stopping = False
        self._tg_offset = 0
        self._reply_target: str | None = None  # peer to send next message to

    async def start(self) -> None:
        logger.info("Starting Telegram peer")
        await asyncio.gather(self._ws_loop(), self._poll_loop())

    async def stop(self) -> None:
        self._stopping = True
        if self._ws:
            await self._ws.close()
        await self._http.aclose()

    # -- Daemon WebSocket --

    async def _ws_loop(self) -> None:
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
                        "path": "/telegram",
                    }))
                    resp = json.loads(await ws.recv())
                    if resp.get("type") != "connected":
                        logger.error("Connect failed: %s", resp)
                        await asyncio.sleep(backoff)
                        continue
                    logger.info("Connected: %s", resp.get("session_id"))
                    async for raw in ws:
                        await self._on_ws(json.loads(raw))
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("WS lost, retry in %.0fs", backoff, exc_info=True)
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _on_ws(self, msg: dict[str, Any]) -> None:
        t = msg.get("type", "")
        who = msg.get("from_peer", "?")
        text = msg.get("text", "")

        if t == "notify":
            await self._tg_send(f"*@{_esc(who)}*\n{_esc(text)}")
        elif t == "query":
            await self._tg_send(f"❓ *@{_esc(who)}*\n{_esc(text)}")
        elif t == "broadcast":
            await self._tg_send(f"📢 *@{_esc(who)}*\n{_esc(text)}")
        elif t == "ping" and self._ws:
            await self._ws.send(json.dumps({"type": "pong"}))

    # -- Telegram polling --

    async def _poll_loop(self) -> None:
        while not self._stopping:
            try:
                r = await self._http.get(
                    f"{self._bot_path}/getUpdates",
                    params={"offset": self._tg_offset, "timeout": 30},
                    timeout=35,
                )
                for u in r.json().get("result", []):
                    self._tg_offset = u["update_id"] + 1
                    await self._on_update(u)
            except asyncio.CancelledError:
                break
            except Exception:
                logger.warning("Poll error", exc_info=True)
                await asyncio.sleep(5)

    async def _on_update(self, u: dict) -> None:
        # Button callback
        cb = u.get("callback_query")
        if cb:
            if str(cb.get("message", {}).get("chat", {}).get("id")) == self._chat_id:
                await self._on_callback(cb)
            return
        # Message
        m = u.get("message", {})
        chat_id = str(m.get("chat", {}).get("id", ""))
        if chat_id != self._chat_id:
            return

        # Photo
        photos = m.get("photo", [])
        if photos:
            caption = m.get("caption", "").strip()
            await self._on_photo(photos[-1], caption, message_id=m.get("message_id"))
            return

        # Text
        text = m.get("text", "")
        if text:
            await self._on_text(text.strip(), message_id=m.get("message_id"))

    async def _on_callback(self, cb: dict) -> None:
        data = cb.get("data", "")
        await self._http.post(
            f"{self._bot_path}/answerCallbackQuery",
            json={"callback_query_id": cb.get("id")},
        )

        if data.startswith(("target:", "notify:")):
            peer = data.split(":", 1)[1]
            self._reply_target = peer
            await self._tg_send(
                f"Now talking to *@{_esc(peer)}*\\. All messages go there\\.",
                _kb([[("📋 Peers", "peers"), ("❌ Clear", "cancel")]]),
            )
        elif data == "cancel":
            self._reply_target = None
            await self._tg_send("Cancelled\\.")
        elif data == "peers":
            await self._cmd_peers()

    async def _on_text(self, text: str, message_id: int | None = None) -> None:
        # Commands
        if text in ("/start", "/peers", "/list"):
            await self._cmd_peers()
            return
        if text == "/clear":
            self._reply_target = None
            await self._tg_send("Cleared\\. No active conversation\\.")
            return
        if text.startswith("/switch ") or text.startswith("/select "):
            peer = text.split(maxsplit=1)[1].strip().lstrip("@")
            self._reply_target = peer
            await self._tg_send(
                f"Now talking to *@{_esc(peer)}*\\. All messages go there\\.",
                _kb([[("📋 Peers", "peers"), ("❌ Clear", "cancel")]]),
            )
            return

        # @peer message — explicit target (also sets sticky)
        m = re.match(r"^@(\S+)\s+(.+)", text, re.DOTALL)
        if m:
            self._reply_target = m.group(1)
            await self._notify(m.group(1), m.group(2), message_id=message_id)
            return

        # Sticky conversation — send to current peer
        if self._reply_target:
            await self._notify(self._reply_target, text, message_id=message_id)
            return

        # No conversation active
        await self._tg_send(
            "No active conversation\\.\n\n"
            "`/peers` — list peers\n"
            "`/select name` — start conversation\n"
            "`@name msg` — quick message"
        )

    async def _on_photo(self, photo: dict, caption: str, message_id: int | None = None) -> None:
        """Handle incoming Telegram photo — upload to daemon, notify peer."""
        if not self._reply_target:
            await self._tg_send(
                "Select a peer first with /select or /peers, then send the photo\\."
            )
            return

        try:
            # Get file path from Telegram
            file_id = photo.get("file_id", "")
            r = await self._http.get(
                f"{self._bot_path}/getFile",
                params={"file_id": file_id},
            )
            file_path = r.json().get("result", {}).get("file_path", "")
            if not file_path:
                await self._tg_send("Failed to get photo from Telegram\\.")
                return

            # Download the photo (need a separate client — self._http has TG base_url)
            async with httpx.AsyncClient() as dl:
                token = self._bot_path.removeprefix("/bot")
                photo_r = await dl.get(
                    f"https://api.telegram.org/file/bot{token}/{file_path}",
                    timeout=15.0,
                )

            # Upload to daemon
            async with httpx.AsyncClient() as ul:
                upload_r = await ul.post(
                    f"{self._daemon_url}/attachments",
                    files={"file": (file_path.split("/")[-1], photo_r.content, "image/jpeg")},
                    timeout=15.0,
                )

            if upload_r.status_code != 200:
                await self._tg_send("Failed to upload photo\\.")
                return

            att = upload_r.json()
            msg = caption or "Photo attached"
            msg += f"\n[Attachment: {att['path']}]"

            await self._notify(self._reply_target, msg, message_id=message_id)
        except Exception as e:
            await self._tg_send(f"Error: {_esc(str(e))}")

    # -- Commands --

    async def _cmd_peers(self) -> None:
        try:
            r = await self._http.get(f"{self._daemon_url}/peers")
            peers = r.json().get("peers", [])
            active = [p for p in peers if p.get("status") in ("online", "busy")]

            if not active:
                await self._tg_send("No peers online\\.")
                return

            lines = []
            buttons = []
            for p in active:
                name = p.get("display_name", p.get("name", "?"))
                path = p.get("path", "")
                folder = Path(path).name or name
                desc = p.get("description", "")
                branch = p.get("metadata", {}).get("branch", "")
                icon = "🟢" if p.get("status") == "online" else "🟡"

                line = f"{icon} *{_esc(folder)}* `{_esc(name)}`"
                if branch:
                    line += f" `{_esc(branch)}`"
                if desc:
                    line += f"\n  _{_esc(desc)}_"
                lines.append(line)
                buttons.append([("💬 " + folder, f"target:{name}")])

            await self._tg_send("\n".join(lines), _kb(buttons))
        except Exception as e:
            await self._tg_send(f"Error: {_esc(str(e))}")

    async def _notify(self, peer: str, message: str, message_id: int | None = None) -> None:
        try:
            r = await self._http.post(
                f"{self._daemon_url}/notify",
                json={
                    "from_peer": self._display_name,
                    "to_peer": peer,
                    "text": message,
                    "bypass_circle": True,
                },
            )
            if r.status_code == 200:
                if message_id:
                    await self._tg_react(message_id)
                # No text reply on success — reaction is the confirmation
            else:
                detail = r.json().get("detail", r.text)
                await self._tg_send(f"✗ {_esc(str(detail))}")
        except Exception as e:
            await self._tg_send(f"Error: {_esc(str(e))}")

    # -- Telegram API --

    async def _tg_react(self, message_id: int, emoji: str = "👍") -> None:
        """Add a reaction to a message (Bot API 7.0+)."""
        try:
            await self._http.post(
                f"{self._bot_path}/setMessageReaction",
                json={
                    "chat_id": self._chat_id,
                    "message_id": message_id,
                    "reaction": [{"type": "emoji", "emoji": emoji}],
                },
            )
        except Exception:
            logger.warning("Telegram react failed", exc_info=True)

    async def _tg_send(self, text: str, markup: dict | None = None) -> None:
        try:
            payload: dict[str, Any] = {
                "chat_id": self._chat_id,
                "text": text,
                "parse_mode": "MarkdownV2",
            }
            if markup:
                payload["reply_markup"] = markup
            await self._http.post(f"{self._bot_path}/sendMessage", json=payload)
        except Exception:
            logger.warning("Telegram send failed", exc_info=True)


def main() -> None:
    """Entry point: repowire telegram start"""
    from repowire.config.models import load_config

    cfg = load_config()
    token = cfg.telegram.bot_token or os.environ.get("TELEGRAM_BOT_TOKEN")
    chat = cfg.telegram.chat_id or os.environ.get("TELEGRAM_CHAT_ID")
    daemon = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

    if not token or not chat:
        print("Set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID,")
        print("or configure in ~/.repowire/config.yaml under 'telegram:'")
        raise SystemExit(1)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    bot = TelegramPeer(bot_token=token, chat_id=chat, daemon_url=daemon)
    try:
        asyncio.run(bot.start())
    except KeyboardInterrupt:
        pass
    finally:
        asyncio.run(bot.stop())
