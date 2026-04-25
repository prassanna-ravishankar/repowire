"""Telegram bot peer for the repowire mesh.

Bridges Telegram <> repowire: notifications become Telegram messages,
Telegram messages become peer notifications. A persistent ReplyKeyboard
below the compose bar lets the user switch target peers with one tap.

Usage:
    TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import time
from collections import deque
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlparse, urlunparse

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.models import DEFAULT_DAEMON_URL

logger = logging.getLogger(__name__)

_MD_ESCAPE_RE = re.compile(r"([_*\[\]()~`>#+=|{}.!\-])")

# Reply-keyboard button markers. Each label is `<marker> <peer_name>`.
CURRENT_MARK = "✦"        # currently selected target
CURRENT_OFF_MARK = "✧"    # currently selected target but peer is offline
RECENT_MARK = "💬"        # recent notifier
MORE_LABEL = "… more"     # open full picker
PEERS_LABEL = "📋 peers"
CLEAR_LABEL = "❌ clear"
RETRY_WINDOW_S = 60.0     # pending-retry TTL after a failed send
PEERS_CACHE_TTL_S = 5.0   # short-lived cache for /peers fetch


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


# -- Pure helpers (unit-testable without Telegram/daemon) --


@dataclass
class PendingRetry:
    text: str
    expires_at: float

    def is_active(self, now: float) -> bool:
        return now < self.expires_at


def compute_visible_recents(
    recents: list[str],
    online: set[str],
    current: str | None,
    limit: int = 5,
) -> list[str]:
    """Return recents filtered to online peers, dedup'd, current excluded.

    Preserves the newest-first order of `recents`.
    """
    seen: set[str] = set()
    if current:
        seen.add(current)
    out: list[str] = []
    for name in recents:
        if name in seen:
            continue
        if name not in online:
            continue
        seen.add(name)
        out.append(name)
        if len(out) >= limit:
            break
    return out


def build_reply_keyboard(
    current: str | None,
    recents: list[str],
    online: set[str],
    pending_retry_text: str | None = None,
) -> dict:
    """Build the persistent ReplyKeyboardMarkup.

    Layout (up to 6 peer slots + commands):
        row 1: current (✦/✧) + 2 recents
        row 2: 3 more recents (or more-picker)
        row 3: 📋 peers · ❌ clear · … more

    Placeholder reflects retry state, then current target, then no-peer.
    """
    visible = compute_visible_recents(recents, online, current, limit=5)

    peer_buttons: list[str] = []
    if current:
        mark = CURRENT_MARK if current in online else CURRENT_OFF_MARK
        peer_buttons.append(f"{mark} {current}")
    peer_buttons.extend(f"{RECENT_MARK} {name}" for name in visible)

    # Split into two rows of up to 3 buttons each.
    row1 = [{"text": t} for t in peer_buttons[:3]]
    row2 = [{"text": t} for t in peer_buttons[3:6]]

    keyboard: list[list[dict[str, str]]] = []
    if row1:
        keyboard.append(row1)
    if row2:
        keyboard.append(row2)
    keyboard.append([
        {"text": PEERS_LABEL},
        {"text": CLEAR_LABEL},
        {"text": MORE_LABEL},
    ])

    if pending_retry_text is not None:
        preview = (
            pending_retry_text
            if len(pending_retry_text) <= 24
            else pending_retry_text[:21] + "…"
        )
        placeholder = f'retry "{preview}" · tap peer to send'
    elif current:
        suffix = " (offline)" if current not in online else ""
        placeholder = f"msg @{current}{suffix}..."
    else:
        placeholder = "No active peer · tap to select"

    return {
        "keyboard": keyboard,
        "resize_keyboard": True,
        "is_persistent": True,
        "input_field_placeholder": placeholder,
    }


def parse_keyboard_tap(text: str) -> tuple[str, str | None]:
    """Classify a free-text message as a keyboard tap or plain text.

    Returns (kind, arg) where kind is one of:
        "select"  → arg is peer name (✦/✧/💬 <name>)
        "peers"   → arg is None (📋 peers tapped)
        "clear"   → arg is None
        "more"    → arg is None (… more tapped)
        "text"    → arg is None (plain user text, not a tap)
    """
    if text == PEERS_LABEL:
        return ("peers", None)
    if text == CLEAR_LABEL:
        return ("clear", None)
    if text == MORE_LABEL:
        return ("more", None)
    for marker in (CURRENT_MARK, CURRENT_OFF_MARK, RECENT_MARK):
        prefix = marker + " "
        if text.startswith(prefix):
            name = text[len(prefix):].strip()
            if name:
                return ("select", name)
    return ("text", None)


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
        self._task: asyncio.Task[None] | None = None
        self._recents: deque[str] = deque(maxlen=10)
        self._pending_retry: PendingRetry | None = None
        self._keyboard_enabled: bool = True
        self._peers_cache: list[dict] | None = None
        self._peers_cache_at: float = 0.0

    async def _run(self) -> None:
        await asyncio.gather(self._ws_loop(), self._poll_loop())

    async def start(self) -> None:
        logger.info("Starting Telegram peer")
        self._stopping = False
        self._task = asyncio.create_task(self._run())
        await self._task

    async def stop(self) -> None:
        self._stopping = True
        if self._ws:
            await self._ws.close()
        await self._http.aclose()
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None

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
                        "role": "service",
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
            self._touch_recent(who)
            await self._tg_send(f"*@{_esc(who)}*\n{_esc(text)}")
        elif t == "query":
            self._touch_recent(who)
            await self._tg_send(f"❓ *@{_esc(who)}*\n{_esc(text)}")
        elif t == "broadcast":
            self._touch_recent(who)
            await self._tg_send(f"📢 *@{_esc(who)}*\n{_esc(text)}")
        elif t == "ping" and self._ws:
            await self._ws.send(json.dumps({"type": "pong"}))

    def _touch_recent(self, peer: str) -> None:
        """Move peer to front of recents; dedup."""
        try:
            self._recents.remove(peer)
        except ValueError:
            pass
        self._recents.appendleft(peer)

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
            await self._select_peer(peer)
        elif data == "cancel":
            self._reply_target = None
            self._pending_retry = None
            await self._tg_send("Cancelled\\.")
        elif data == "peers":
            await self._cmd_peers()

    async def _on_text(self, text: str, message_id: int | None = None) -> None:
        # Reply-keyboard taps
        kind, arg = parse_keyboard_tap(text)
        if kind == "select" and arg:
            await self._select_peer(arg, message_id=message_id, trigger_retry=True)
            return
        if kind == "peers":
            await self._cmd_peers()
            return
        if kind == "clear":
            self._reply_target = None
            self._pending_retry = None
            await self._tg_send("Cleared\\. No active conversation\\.")
            return
        if kind == "more":
            await self._cmd_peers()
            return

        # Slash commands
        if text in ("/start", "/peers", "/list"):
            await self._cmd_peers()
            return
        if text == "/clear":
            self._reply_target = None
            self._pending_retry = None
            await self._tg_send("Cleared\\. No active conversation\\.")
            return
        if text == "/keyboard off":
            self._keyboard_enabled = False
            await self._http.post(
                f"{self._bot_path}/sendMessage",
                json={
                    "chat_id": self._chat_id,
                    "text": "Keyboard hidden\\. Use `/keyboard on` to restore\\.",
                    "parse_mode": "MarkdownV2",
                    "reply_markup": {"remove_keyboard": True},
                },
            )
            return
        if text == "/keyboard on":
            self._keyboard_enabled = True
            await self._tg_send("Keyboard restored\\.")
            return
        if text.startswith("/switch ") or text.startswith("/select "):
            peer = text.split(maxsplit=1)[1].strip().lstrip("@")
            await self._select_peer(peer, message_id=message_id)
            return

        # @peer message — explicit target (also sets sticky)
        m = re.match(r"^@(\S+)\s+(.+)", text, re.DOTALL)
        if m:
            self._reply_target = m.group(1)
            self._pending_retry = None  # typing cancels retry
            await self._notify(m.group(1), m.group(2), message_id=message_id)
            return

        # Sticky conversation — send to current peer
        if self._reply_target:
            self._pending_retry = None  # typing cancels retry
            await self._notify(self._reply_target, text, message_id=message_id)
            return

        # No conversation active
        self._pending_retry = None
        await self._tg_send(
            "No active conversation\\.\n\n"
            "`/peers` — list peers\n"
            "`/select name` — start conversation\n"
            "`@name msg` — quick message"
        )

    async def _select_peer(
        self,
        peer: str,
        message_id: int | None = None,
        trigger_retry: bool = False,
    ) -> None:
        """Switch target peer. If trigger_retry and a retry is pending, replay it.

        Retry replay is opt-in (only keyboard taps set trigger_retry=True) so
        that slash commands like /switch don't surprise the user by resending.
        """
        peer = peer.lstrip("@")
        retry = self._pending_retry
        if trigger_retry and retry and retry.is_active(time.monotonic()):
            self._reply_target = peer
            self._pending_retry = None
            await self._notify(peer, retry.text, message_id=message_id)
            return

        self._reply_target = peer
        self._pending_retry = None
        await self._tg_send(f"Now talking to *@{_esc(peer)}*\\.")

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

    async def _fetch_online_peers(self, *, use_cache: bool = True) -> list[dict]:
        """Fetch current peers from daemon. Empty list on failure.

        Caches for PEERS_CACHE_TTL_S to avoid hammering the daemon when
        many bot messages fire in quick succession (each rebuilds the
        keyboard). Pass use_cache=False for user-driven listings.
        """
        now = time.monotonic()
        if (
            use_cache
            and self._peers_cache is not None
            and now - self._peers_cache_at < PEERS_CACHE_TTL_S
        ):
            return self._peers_cache
        try:
            r = await self._http.get(f"{self._daemon_url}/peers")
            peers = r.json().get("peers", [])
            self._peers_cache = peers
            self._peers_cache_at = now
            return peers
        except Exception:
            logger.warning("Failed to fetch peers", exc_info=True)
            return []

    async def _cmd_peers(self) -> None:
        peers = await self._fetch_online_peers(use_cache=False)
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
            buttons.append(("💬 " + folder, f"target:{name}"))

        # 2-column grid when >4 peers; 1-column otherwise.
        if len(buttons) > 4:
            rows = [buttons[i:i + 2] for i in range(0, len(buttons), 2)]
        else:
            rows = [[b] for b in buttons]

        await self._tg_send("\n".join(lines), markup=_kb(rows))

    async def _notify(self, peer: str, message: str, message_id: int | None = None) -> None:
        try:
            r = await self._http.post(
                f"{self._daemon_url}/notify",
                json={
                    "from_peer": self._display_name,
                    "to_peer": peer,
                    "text": message,
                },
            )
            if r.status_code == 200:
                if message_id:
                    await self._tg_react(message_id)
                # No text reply on success — reaction is the confirmation
            else:
                detail = r.json().get("detail", r.text)
                self._pending_retry = PendingRetry(
                    text=message,
                    expires_at=time.monotonic() + RETRY_WINDOW_S,
                )
                await self._tg_send(
                    f"✗ Couldn't reach *@{_esc(peer)}*: {_esc(str(detail))}\n"
                    f"Tap a peer to resend, or type a new message to cancel\\."
                )
        except Exception as e:
            self._pending_retry = PendingRetry(
                text=message,
                expires_at=time.monotonic() + RETRY_WINDOW_S,
            )
            await self._tg_send(
                f"⚠️ Daemon unreachable: {_esc(str(e))}\n"
                f"Tap a peer to retry, or type a new message to cancel\\."
            )

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
            if markup is not None:
                payload["reply_markup"] = markup
            elif self._keyboard_enabled:
                payload["reply_markup"] = await self._current_reply_keyboard()
            await self._http.post(f"{self._bot_path}/sendMessage", json=payload)
        except Exception:
            logger.warning("Telegram send failed", exc_info=True)

    async def _current_reply_keyboard(self) -> dict:
        """Build the persistent keyboard from fresh peer data."""
        peers = await self._fetch_online_peers()
        online = {
            p.get("display_name", p.get("name", ""))
            for p in peers
            if p.get("status") in ("online", "busy")
        }
        online.discard("")
        pending = (
            self._pending_retry.text
            if self._pending_retry and self._pending_retry.is_active(time.monotonic())
            else None
        )
        return build_reply_keyboard(
            current=self._reply_target,
            recents=list(self._recents),
            online=online,
            pending_retry_text=pending,
        )


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
