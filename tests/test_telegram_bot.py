"""Unit tests for Telegram bot keyboard helpers + routing logic."""

from __future__ import annotations

import time
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from repowire.telegram.bot import (
    CLEAR_LABEL,
    CURRENT_MARK,
    CURRENT_OFF_MARK,
    MORE_LABEL,
    PEERS_LABEL,
    RECENT_MARK,
    PendingRetry,
    TelegramPeer,
    build_reply_keyboard,
    compute_visible_recents,
    parse_keyboard_tap,
    parse_notify_command,
)

# -- compute_visible_recents --


def test_recents_preserves_newest_first_order():
    recents = ["c", "b", "a"]
    online = {"a", "b", "c"}
    assert compute_visible_recents(recents, online, current=None) == ["c", "b", "a"]


def test_recents_filters_offline_peers():
    recents = ["c", "b", "a"]
    online = {"a", "c"}
    assert compute_visible_recents(recents, online, current=None) == ["c", "a"]


def test_recents_excludes_current_peer():
    recents = ["c", "b", "a"]
    online = {"a", "b", "c"}
    assert compute_visible_recents(recents, online, current="b") == ["c", "a"]


def test_recents_dedups_repeated_names():
    recents = ["a", "b", "a", "c", "b"]
    online = {"a", "b", "c"}
    assert compute_visible_recents(recents, online, current=None) == ["a", "b", "c"]


def test_recents_honors_limit():
    recents = ["e", "d", "c", "b", "a"]
    online = {"a", "b", "c", "d", "e"}
    assert compute_visible_recents(recents, online, current=None, limit=3) == ["e", "d", "c"]


# -- build_reply_keyboard --


def test_keyboard_marks_current_online():
    kb = build_reply_keyboard(current="torale", recents=[], online={"torale"})
    first_button = kb["keyboard"][0][0]["text"]
    assert first_button == f"{CURRENT_MARK} torale"
    assert kb["input_field_placeholder"] == "msg @torale..."


def test_keyboard_marks_current_offline():
    kb = build_reply_keyboard(current="torale", recents=[], online=set())
    first_button = kb["keyboard"][0][0]["text"]
    assert first_button == f"{CURRENT_OFF_MARK} torale"
    assert "offline" in kb["input_field_placeholder"]


def test_keyboard_no_current_shows_empty_placeholder():
    kb = build_reply_keyboard(current=None, recents=[], online=set())
    assert "No active peer" in kb["input_field_placeholder"]


def test_keyboard_recents_appear_after_current():
    kb = build_reply_keyboard(
        current="torale",
        recents=["orch", "repowire"],
        online={"torale", "orch", "repowire"},
    )
    labels = [btn["text"] for row in kb["keyboard"] for btn in row]
    assert labels[0] == f"{CURRENT_MARK} torale"
    assert labels[1] == f"{RECENT_MARK} orch"
    assert labels[2] == f"{RECENT_MARK} repowire"


def test_keyboard_always_has_commands_row():
    kb = build_reply_keyboard(current=None, recents=[], online=set())
    last_row_labels = [btn["text"] for btn in kb["keyboard"][-1]]
    assert PEERS_LABEL in last_row_labels
    assert CLEAR_LABEL in last_row_labels
    assert MORE_LABEL in last_row_labels


def test_keyboard_pending_retry_placeholder():
    kb = build_reply_keyboard(
        current="torale",
        recents=[],
        online={"torale"},
        pending_retry_text="do option B",
    )
    assert "retry" in kb["input_field_placeholder"]
    assert "do option B" in kb["input_field_placeholder"]


def test_keyboard_pending_retry_truncates_long_text():
    long_text = "x" * 100
    kb = build_reply_keyboard(
        current=None,
        recents=[],
        online=set(),
        pending_retry_text=long_text,
    )
    assert "…" in kb["input_field_placeholder"]
    assert len(kb["input_field_placeholder"]) < 60


def test_keyboard_is_persistent_and_resized():
    kb = build_reply_keyboard(current=None, recents=[], online=set())
    assert kb["is_persistent"] is True
    assert kb["resize_keyboard"] is True


# -- parse_keyboard_tap --


def test_parse_current_peer_tap():
    assert parse_keyboard_tap(f"{CURRENT_MARK} torale") == ("select", "torale")


def test_parse_current_offline_peer_tap():
    assert parse_keyboard_tap(f"{CURRENT_OFF_MARK} torale") == ("select", "torale")


def test_parse_recent_peer_tap():
    assert parse_keyboard_tap(f"{RECENT_MARK} orch") == ("select", "orch")


def test_parse_peers_label():
    assert parse_keyboard_tap(PEERS_LABEL) == ("peers", None)


def test_parse_clear_label():
    assert parse_keyboard_tap(CLEAR_LABEL) == ("clear", None)


def test_parse_more_label():
    assert parse_keyboard_tap(MORE_LABEL) == ("more", None)


def test_parse_plain_text_is_text():
    assert parse_keyboard_tap("hello there") == ("text", None)


def test_parse_at_mention_is_text():
    # @-prefixed text should fall through to the @peer regex path, not be a tap
    assert parse_keyboard_tap("@torale do it") == ("text", None)


def test_parse_marker_alone_without_name_is_text():
    assert parse_keyboard_tap(CURRENT_MARK) == ("text", None)
    assert parse_keyboard_tap(f"{CURRENT_MARK} ") == ("text", None)


# -- explicit notify/FYI command parsing --


def test_parse_notify_command_with_peer():
    assert parse_notify_command("/notify @agent build passed") == ("agent", "build passed")


def test_parse_fyi_command_without_peer_uses_sticky_target():
    assert parse_notify_command("/fyi build passed") == (None, "build passed")


def test_parse_notify_command_ignores_regular_text():
    assert parse_notify_command("notify @agent build passed") is None


# -- inbound routing --


@pytest.fixture
def telegram_bot() -> TelegramPeer:
    return TelegramPeer(
        bot_token="test-token",
        chat_id="0",
        daemon_url="http://127.0.0.1:0",
        display_name="telegram",
        circle="default",
    )


@pytest.mark.asyncio
async def test_at_peer_text_opens_ask_by_default(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_ask", ask)
    monkeypatch.setattr(telegram_bot, "_notify", notify)

    await telegram_bot._on_text("@agent please check", message_id=42)

    ask.assert_awaited_once_with("agent", "please check", message_id=42)
    notify.assert_not_awaited()
    assert telegram_bot._reply_target == "agent"


@pytest.mark.asyncio
async def test_sticky_text_opens_ask_by_default(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    telegram_bot._reply_target = "agent"
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_ask", ask)
    monkeypatch.setattr(telegram_bot, "_notify", notify)

    await telegram_bot._on_text("please check", message_id=42)

    ask.assert_awaited_once_with("agent", "please check", message_id=42)
    notify.assert_not_awaited()


@pytest.mark.asyncio
async def test_notify_command_keeps_fire_and_forget(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_ask", ask)
    monkeypatch.setattr(telegram_bot, "_notify", notify)

    await telegram_bot._on_text("/notify @agent build passed", message_id=42)

    notify.assert_awaited_once_with("agent", "build passed", message_id=42)
    ask.assert_not_awaited()
    assert telegram_bot._reply_target == "agent"


@pytest.mark.asyncio
async def test_send_peer_message_uses_daemon_assigned_service_name(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Ack replies need the real registered service name as from_peer."""
    telegram_bot._display_name = "telegram-claude-code"
    post = AsyncMock()
    post.return_value = SimpleNamespace(status_code=200)
    monkeypatch.setattr(telegram_bot._http, "post", post)

    await telegram_bot._ask("agent", "please check")

    post.assert_awaited_once()
    assert post.await_args.kwargs["json"]["from_peer"] == "telegram-claude-code"
    assert post.await_args.kwargs["json"]["to_peer"] == "agent"


@pytest.mark.asyncio
async def test_send_peer_message_includes_attachments(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    post = AsyncMock()
    post.return_value = SimpleNamespace(status_code=200)
    monkeypatch.setattr(telegram_bot._http, "post", post)

    await telegram_bot._ask(
        "agent",
        "see image",
        attachments=[{
            "id": "att123",
            "path": "/tmp/att123.png",
            "filename": "diagram.png",
        }],
    )

    assert post.await_args.kwargs["json"]["attachments"][0]["id"] == "att123"


@pytest.mark.asyncio
async def test_peer_picker_targets_peer_id_for_duplicate_names(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_tg_send", send)
    monkeypatch.setattr(
        telegram_bot,
        "_fetch_online_peers",
        AsyncMock(return_value=[
            {
                "peer_id": "repow-default",
                "display_name": "repowire-codex",
                "status": "busy",
                "circle": "default",
                "path": "/projects/repowire",
            },
            {
                "peer_id": "repow-66",
                "display_name": "repowire-codex",
                "status": "online",
                "circle": "66",
                "path": "/projects/repowire",
            },
        ]),
    )

    await telegram_bot._cmd_peers()

    markup = send.await_args.kwargs["markup"]
    buttons = [button for row in markup["inline_keyboard"] for button in row]
    assert {button["callback_data"] for button in buttons} == {
        "target:repow-default",
        "target:repow-66",
    }
    assert {button["text"] for button in buttons} == {
        "💬 repowire · default",
        "💬 repowire · 66",
    }


@pytest.mark.asyncio
async def test_retry_error_offers_peer_id_buttons(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    post = AsyncMock()
    post.return_value = SimpleNamespace(
        status_code=409,
        json=lambda: {
            "detail": (
                "Ambiguous peer name 'repowire-codex': matches in circles "
                "['66', 'default']."
            ),
        },
        text="ambiguous",
    )
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot._http, "post", post)
    monkeypatch.setattr(telegram_bot, "_tg_send", send)
    monkeypatch.setattr(
        telegram_bot,
        "_fetch_online_peers",
        AsyncMock(return_value=[
            {
                "peer_id": "repow-default",
                "display_name": "repowire-codex",
                "status": "busy",
                "circle": "default",
                "path": "/projects/repowire",
            },
            {
                "peer_id": "repow-66",
                "display_name": "repowire-codex",
                "status": "online",
                "circle": "66",
                "path": "/projects/repowire",
            },
        ]),
    )

    await telegram_bot._ask("repowire-codex", "ping")

    markup = send.await_args.kwargs["markup"]
    buttons = [button for row in markup["inline_keyboard"] for button in row]
    assert {button["callback_data"] for button in buttons} == {
        "target:repow-default",
        "target:repow-66",
    }
    assert telegram_bot._pending_retry is not None


# -- PendingRetry TTL --


def test_pending_retry_active_within_window():
    now = time.monotonic()
    r = PendingRetry(text="hi", expires_at=now + 10)
    assert r.is_active(now) is True
    assert r.is_active(now + 5) is True


def test_pending_retry_expired_after_window():
    now = time.monotonic()
    r = PendingRetry(text="hi", expires_at=now + 10)
    assert r.is_active(now + 11) is False


def test_pending_retry_exactly_at_expiry_is_inactive():
    now = time.monotonic()
    r = PendingRetry(text="hi", expires_at=now + 10)
    assert r.is_active(now + 10) is False
