"""Telegram bot peer for the repowire mesh.

Bridges Telegram <> repowire: notifications become Telegram messages,
Telegram messages become peer asks by default. A persistent ReplyKeyboard
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
from typing import Any, Literal, cast
from urllib.parse import quote, urlparse, urlunparse

import httpx
import websockets
from websockets.asyncio.client import ClientConnection

from repowire.config.models import DEFAULT_DAEMON_URL

logger = logging.getLogger(__name__)

_MD_ESCAPE_RE = re.compile(r"([_*\[\]()~`>#+=|{}.!\-])")

# Reply-keyboard button markers. Each label is `<marker> <peer_name>`.
_MAX_REMEMBERED_QUESTIONS = 200  # cap the cid->option-ids map (FIFO eviction)

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
    mode: Literal["ask", "notify"] = "ask"
    attachments: list[dict[str, Any]] | None = None

    def is_active(self, now: float) -> bool:
        return now < self.expires_at


@dataclass
class OrchestratorStatus:
    present: bool
    peer: str | None = None


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


def parse_notify_command(text: str) -> tuple[str | None, str] | None:
    """Parse explicit fire-and-forget commands.

    Supported forms:
        /notify @peer message
        /notify message       (uses sticky target)
        /fyi @peer message
        /fyi message          (uses sticky target)
    """
    m = re.match(r"^/(?:notify|fyi)(?:\s+(.+))?$", text, re.DOTALL | re.IGNORECASE)
    if not m:
        return None
    body = (m.group(1) or "").strip()
    if not body:
        return (None, "")
    target = re.match(r"^@(\S+)\s+(.+)", body, re.DOTALL)
    if target:
        return (target.group(1), target.group(2))
    return (None, body)


def _looks_like_orchestrator_target(target: str) -> bool:
    normalized = target.lstrip("@").lower()
    return normalized == "orchestrator" or normalized.startswith("orchestrator-")


def _orchestrator_peer_from_list(
    peers: list[dict[str, Any]],
    *,
    circle: str,
) -> str | None:
    active = [
        p for p in peers
        if p.get("status") in ("online", "busy")
        and p.get("circle", circle) == circle
    ]
    role_matches = [p for p in active if p.get("role") == "orchestrator"]
    heuristic_matches = [
        p for p in peers
        if p.get("circle", circle) == circle
        and (
            str(p.get("display_name") or p.get("name") or "").startswith("orchestrator-")
            or Path(str(p.get("path") or "")).name == "orchestrator"
        )
    ]
    for peer in role_matches + heuristic_matches:
        target = peer.get("peer_id") or peer.get("display_name") or peer.get("name")
        if target:
            return str(target)
    return None


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
        self._peer_id: str | None = None
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
        # cid -> ordered option ids, so an index-based answer callback (kept
        # short for Telegram's 64-byte callback_data limit) maps to a real id.
        self._question_options: dict[str, list[str]] = {}
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
                    assigned_name = resp.get("display_name")
                    if isinstance(assigned_name, str) and assigned_name:
                        self._display_name = assigned_name
                    assigned_peer_id = resp.get("peer_id") or resp.get("session_id")
                    if isinstance(assigned_peer_id, str) and assigned_peer_id:
                        self._peer_id = assigned_peer_id
                    logger.info("Connected: %s", resp.get("session_id"))
                    # Best-effort: route user messages to the orchestrator by
                    # default if one is registered. User can still /select.
                    await self._seed_default_target_from_orchestrator()
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

    async def _seed_default_target_from_orchestrator(self) -> None:
        """If an orchestrator peer is registered and no _reply_target is set,
        seed _reply_target to the orchestrator's display_name so user-typed
        messages route there by default. User can still /select another peer.
        """
        if self._reply_target:
            return  # respect explicit prior selection
        status = await self._fetch_orchestrator_status(use_cache=False)
        if not status.present or not status.peer:
            return
        self._reply_target = status.peer
        logger.info("Default reply target seeded to orchestrator: %s", status.peer)

    async def _on_ws(self, msg: dict[str, Any]) -> None:
        t = msg.get("type", "")
        who = msg.get("from_peer", "?")
        text = msg.get("text", "")
        attachments = msg.get("attachments", [])

        if t == "notify":
            self._touch_recent(who)
            await self._tg_send(f"*@{_esc(who)}*\n{_esc(text)}")
            await self._tg_send_attachments(attachments)
        elif t == "query":
            self._touch_recent(who)
            await self._tg_send(f"❓ *@{_esc(who)}*\n{_esc(text)}")
        elif t == "ask":
            self._touch_recent(who)
            cid = msg.get("correlation_id", "")
            short_cid = cid[:12] if cid else "?"
            markup = self._ask_markup(cid, msg.get("question")) if cid else None
            await self._tg_send(
                f"❓ *@{_esc(who)}* `[ask #{_esc(short_cid)}]`\n{_esc(text)}",
                markup=markup,
            )
            await self._tg_send_attachments(attachments)
        elif t == "broadcast":
            self._touch_recent(who)
            await self._tg_send(f"📢 *@{_esc(who)}*\n{_esc(text)}")
            await self._tg_send_attachments(attachments)
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
            await self._select_peer(peer, trigger_retry=True)
        elif data == "cancel":
            self._reply_target = None
            self._pending_retry = None
            await self._tg_send("Cancelled\\.")
        elif data == "peers":
            await self._cmd_peers()
        elif data.startswith("answer:"):
            _, cid, option_index = data.split(":", 2)
            await self._answer_choice(cid, option_index)
        elif data.startswith("deny:"):
            await self._deny_question(data.split(":", 1)[1])
        elif data.startswith("ack:"):
            cid = data.split(":", 1)[1]
            await self._ack_ask(cid)

    def _ask_markup(self, cid: str, question: dict | None) -> dict | None:
        """Inline keyboard for an ask.

        A choice question renders one button per option (callback answer:cid:idx,
        index-based to stay within Telegram's 64-byte callback_data limit);
        anything else (plain ask, text, acknowledge) gets the bare Ack button.
        """
        if isinstance(question, dict) and question.get("kind") == "choice":
            options = question.get("options") or []
            if options:
                # Remember the option ids for this cid so the index-based
                # callback can map back to the real option_id. Bounded + FIFO so
                # the map can't grow unbounded over a long-lived bot process
                # (lazy cleanup, no timer — codex review).
                if len(self._question_options) >= _MAX_REMEMBERED_QUESTIONS:
                    self._question_options.pop(next(iter(self._question_options)), None)
                self._question_options[cid] = [
                    str(o.get("id")) for o in options if isinstance(o, dict)
                ]
                rows = [
                    [(str(o.get("title") or o.get("id")), f"answer:{cid}:{i}")]
                    for i, o in enumerate(options)
                    if isinstance(o, dict)
                ]
                # ACP tool-permission options are allow-only — add an explicit
                # Deny that posts outcome:"denied" (no option).
                if question.get("scope") == "tool_permission":
                    rows.append([("✕ Deny", f"deny:{cid}")])
                return _kb(rows)
        return _kb([[("✓ Ack", f"ack:{cid}")]])

    async def _deny_question(self, cid: str) -> None:
        """Deny a tool-permission question → POST /answer {outcome: denied}."""
        try:
            r = await self._http.post(
                f"{self._daemon_url}/answer",
                json={"correlation_id": cid, "outcome": "denied"},
                timeout=5.0,
            )
            short = cid[:12]
            if r.status_code == 200:
                self._question_options.pop(cid, None)
                await self._tg_send(f"✕ Denied `#{_esc(short)}`")
            elif r.status_code == 410:
                await self._tg_send(f"`#{_esc(short)}` already answered")
            else:
                await self._tg_send(f"Deny failed for `#{_esc(short)}`: {r.status_code}")
        except Exception as e:
            await self._tg_send(f"Deny error: {_esc(str(e))}")

    async def _answer_choice(self, cid: str, option_index_raw: str) -> None:
        """Answer a choice question by option index → POST /answer."""
        ids = self._question_options.get(cid, [])
        try:
            option_id = ids[int(option_index_raw)]
        except (ValueError, IndexError):
            self._question_options.pop(cid, None)  # stale entry — don't retain
            await self._tg_send(f"Stale or unknown option for `#{_esc(cid[:12])}`")
            return
        try:
            r = await self._http.post(
                f"{self._daemon_url}/answer",
                json={"correlation_id": cid, "option_id": option_id},
                timeout=5.0,
            )
            short = cid[:12]
            if r.status_code == 200:
                self._question_options.pop(cid, None)
                await self._tg_send(f"✓ Answered `#{_esc(short)}`: {_esc(option_id)}")
            elif r.status_code == 410:
                await self._tg_send(f"`#{_esc(short)}` already answered")
            else:
                await self._tg_send(f"Answer failed for `#{_esc(short)}`: {r.status_code}")
        except Exception as e:
            await self._tg_send(f"Answer error: {_esc(str(e))}")

    async def _ack_ask(self, correlation_id: str) -> None:
        """Bare-ack an open ask. Uses the bot's configured display name."""
        try:
            r = await self._http.post(
                f"{self._daemon_url}/ack",
                json={
                    "correlation_id": correlation_id,
                    "from_peer": self._display_name,
                },
                timeout=5.0,
            )
            short = correlation_id[:12]
            if r.status_code == 200:
                await self._tg_send(f"✓ Acked `#{_esc(short)}`")
            elif r.status_code == 404:
                await self._tg_send(f"`#{_esc(short)}` already closed or unknown")
            else:
                await self._tg_send(f"Ack failed for `#{_esc(short)}`: {r.status_code}")
        except Exception as e:
            await self._tg_send(f"Ack error: {_esc(str(e))}")

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
        notify_cmd = parse_notify_command(text)
        if notify_cmd is not None:
            peer, message = notify_cmd
            if not message:
                await self._tg_send("Usage: `/notify [@peer] message` or `/fyi [@peer] message`")
                return
            target = peer or self._reply_target
            if not target:
                await self._tg_send(
                    "No active peer\\. Use `/notify @peer message` "
                    "or `/select peer` first\\."
                )
                return
            if peer:
                self._reply_target = peer
            self._pending_retry = None  # typing cancels retry
            await self._notify(target, message, message_id=message_id)
            return

        # @peer message — explicit target (also sets sticky). Human inbound
        # messages default to ask/ack so the agent is reminded until it replies.
        m = re.match(r"^@(\S+)\s+(.+)", text, re.DOTALL)
        if m:
            self._reply_target = m.group(1)
            self._pending_retry = None  # typing cancels retry
            await self._ask(m.group(1), m.group(2), message_id=message_id)
            return

        # Sticky conversation — ask current peer by default
        if self._reply_target:
            self._pending_retry = None  # typing cancels retry
            await self._ask(self._reply_target, text, message_id=message_id)
            return

        # No conversation active
        self._pending_retry = None
        orchestrator = await self._fetch_orchestrator_status()
        if not orchestrator.present:
            await self._send_no_orchestrator()
            return
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
            await self._send_peer_message(
                peer,
                retry.text,
                message_id=message_id,
                mode=retry.mode,
                attachments=retry.attachments,
            )
            return

        self._reply_target = peer
        self._pending_retry = None
        label = await self._peer_label_for_target(peer)
        await self._tg_send(f"Now talking to *@{_esc(label)}*\\.")

    async def _on_photo(self, photo: dict, caption: str, message_id: int | None = None) -> None:
        """Handle incoming Telegram photo — upload to daemon, ask peer."""
        if not self._reply_target:
            orchestrator = await self._fetch_orchestrator_status()
            if not orchestrator.present:
                await self._send_no_orchestrator()
                return
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

            await self._ask(
                self._reply_target,
                msg,
                message_id=message_id,
                attachments=[att],
            )
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
            peers = [p for p in peers if not self._is_self_peer(p)]
            self._peers_cache = peers
            self._peers_cache_at = now
            return peers
        except Exception:
            logger.warning("Failed to fetch peers", exc_info=True)
            return []

    def _is_self_peer(self, peer: dict[str, Any]) -> bool:
        identifiers = {
            str(peer.get("peer_id") or ""),
            str(peer.get("display_name") or ""),
            str(peer.get("name") or ""),
        }
        self_identifiers = {self._display_name, self._peer_id}
        if identifiers & {i for i in self_identifiers if i}:
            return True
        return peer.get("role") == "service" and peer.get("path") == "/telegram"

    async def _fetch_orchestrator_status(
        self, *, use_cache: bool = True,
    ) -> OrchestratorStatus:
        """Return route-backed orchestrator presence for this bot's circle.

        The daemon route is landing in a sibling branch. Until it is present,
        fall back to the old peer-list scan so this surface remains testable.
        """
        try:
            r = await self._http.get(
                f"{self._daemon_url}/circles/{quote(self._circle, safe='')}/orchestrator",
                timeout=5.0,
            )
            if r.status_code == 200:
                data = r.json()
                peer = (
                    data.get("peer_id")
                    or data.get("peer_name")
                    or data.get("display_name")
                    or data.get("name")
                )
                if data.get("present"):
                    return OrchestratorStatus(
                        present=True,
                        peer=str(peer) if peer else None,
                    )
        except Exception:
            logger.debug("Failed to fetch orchestrator status route", exc_info=True)

        peers = await self._fetch_online_peers(use_cache=use_cache)
        peer = _orchestrator_peer_from_list(peers, circle=self._circle)
        if not peer:
            return OrchestratorStatus(present=False)
        return OrchestratorStatus(present=True, peer=peer)

    async def _send_no_orchestrator(self) -> None:
        await self._tg_send(
            f"No orchestrator online in circle `{_esc(self._circle)}`\\.\n"
            "Spawn one with `repowire orchestrator start`\\."
        )

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
            circle = p.get("circle", "")
            icon = "🟢" if p.get("status") == "online" else "🟡"
            target = self._peer_target(p)

            line = f"{icon} *{_esc(folder)}* `{_esc(name)}`"
            if circle:
                line += f" `{_esc(circle)}`"
            if branch:
                line += f" `{_esc(branch)}`"
            if desc:
                line += f"\n  _{_esc(desc)}_"
            lines.append(line)
            buttons.append(("💬 " + self._peer_button_label(p), f"target:{target}"))

        # 2-column grid when >4 peers; 1-column otherwise.
        if len(buttons) > 4:
            rows = [buttons[i:i + 2] for i in range(0, len(buttons), 2)]
        else:
            rows = [[b] for b in buttons]

        await self._tg_send("\n".join(lines), markup=_kb(rows))

    def _peer_target(self, peer: dict[str, Any]) -> str:
        target = peer.get("peer_id") or peer.get("display_name") or peer.get("name") or "?"
        return str(target)

    def _peer_button_label(self, peer: dict[str, Any]) -> str:
        name = str(peer.get("display_name") or peer.get("name") or "?")
        path = str(peer.get("path") or "")
        folder = Path(path).name or name
        circle = peer.get("circle")
        return f"{folder} · {circle}" if circle else folder

    async def _peer_label_for_target(self, target: str) -> str:
        peers = await self._fetch_online_peers()
        for peer in peers:
            identifiers = {
                str(peer.get("peer_id") or ""),
                str(peer.get("display_name") or ""),
                str(peer.get("name") or ""),
            }
            if target in identifiers:
                name = str(peer.get("display_name") or peer.get("name") or target)
                circle = peer.get("circle")
                return f"{name} ({circle})" if circle else name
        return target

    async def _peer_retry_markup(self) -> dict | None:
        peers = await self._fetch_online_peers(use_cache=False)
        active = [p for p in peers if p.get("status") in ("online", "busy")]
        if not active:
            return None
        buttons = [
            ("💬 " + self._peer_button_label(peer), f"target:{self._peer_target(peer)}")
            for peer in active
        ]
        rows = [buttons[i:i + 2] for i in range(0, len(buttons), 2)]
        return _kb(rows)

    async def _ask(
        self,
        peer: str,
        message: str,
        message_id: int | None = None,
        attachments: list[dict[str, Any]] | None = None,
    ) -> None:
        await self._send_peer_message(
            peer, message, message_id=message_id, mode="ask", attachments=attachments,
        )

    async def _notify(
        self,
        peer: str,
        message: str,
        message_id: int | None = None,
        attachments: list[dict[str, Any]] | None = None,
    ) -> None:
        await self._send_peer_message(
            peer, message, message_id=message_id, mode="notify", attachments=attachments,
        )

    async def _send_peer_message(
        self,
        peer: str,
        message: str,
        message_id: int | None = None,
        *,
        mode: Literal["ask", "notify"],
        attachments: list[dict[str, Any]] | None = None,
    ) -> None:
        if _looks_like_orchestrator_target(peer):
            status = await self._fetch_orchestrator_status(use_cache=False)
            if status.present and status.peer:
                peer = status.peer
                self._reply_target = peer
        endpoint = "ask" if mode == "ask" else "notify"
        body: dict[str, Any] = {
            "from_peer": self._display_name,
            "to_peer": peer,
            "text": message,
        }
        if attachments:
            body["attachments"] = attachments
        try:
            r = await self._http.post(
                f"{self._daemon_url}/{endpoint}",
                json=body,
            )
            if r.status_code == 200:
                if message_id:
                    await self._tg_react(message_id, emoji="👍")
                # No text reply on success — reaction is the confirmation
            else:
                try:
                    detail = r.json().get("detail", r.text)
                except Exception:
                    detail = r.text
                self._pending_retry = PendingRetry(
                    text=message,
                    expires_at=time.monotonic() + RETRY_WINDOW_S,
                    mode=mode,
                    attachments=attachments,
                )
                markup = await self._peer_retry_markup()
                await self._tg_send(
                    f"✗ Couldn't reach *@{_esc(peer)}*: {_esc(str(detail))}\n"
                    f"Tap a peer to resend, or type a new message to cancel\\.",
                    markup=markup,
                )
        except Exception as e:
            self._pending_retry = PendingRetry(
                text=message,
                expires_at=time.monotonic() + RETRY_WINDOW_S,
                mode=mode,
                attachments=attachments,
            )
            markup = await self._peer_retry_markup()
            await self._tg_send(
                f"⚠️ Daemon unreachable: {_esc(str(e))}\n"
                f"Tap a peer to retry, or type a new message to cancel\\.",
                markup=markup,
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

    async def _tg_send_attachments(self, attachments: object) -> None:
        if not isinstance(attachments, list):
            return
        for attachment in attachments:
            if not isinstance(attachment, dict):
                continue
            attachment_data = cast(dict[str, Any], attachment)
            path = attachment_data.get("path")
            attachment_id = attachment_data.get("id")
            filename = attachment_data.get("filename") or (
                Path(path).name if isinstance(path, str) else "attachment"
            )
            content_type = str(attachment_data.get("content_type") or "")
            try:
                if isinstance(path, str) and Path(path).exists():
                    method = "sendPhoto" if content_type.startswith("image/") else "sendDocument"
                    field = "photo" if method == "sendPhoto" else "document"
                    with open(path, "rb") as fh:
                        await self._http.post(
                            f"{self._bot_path}/{method}",
                            data={"chat_id": self._chat_id},
                            files={
                                field: (
                                    filename,
                                    fh,
                                    content_type or "application/octet-stream",
                                )
                            },
                        )
                    continue
                if attachment_id:
                    await self._tg_send(
                        f"📎 {_esc(str(filename))}\n"
                        f"{_esc(self._daemon_url + '/attachments/' + str(attachment_id))}"
                    )
            except Exception:
                logger.warning("Telegram attachment send failed", exc_info=True)

    async def _current_reply_keyboard(self) -> dict:
        """Build the persistent keyboard from fresh peer data."""
        peers = await self._fetch_online_peers()
        online: set[str] = set()
        for p in peers:
            if p.get("status") not in ("online", "busy"):
                continue
            for key in ("peer_id", "display_name", "name"):
                value = p.get(key)
                if value:
                    online.add(str(value))
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
