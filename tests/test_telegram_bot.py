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


@pytest.mark.parametrize(
    ("recents", "online", "current", "limit", "expected"),
    [
        (["c", "b", "a"], {"a", "b", "c"}, None, 5, ["c", "b", "a"]),
        (["c", "b", "a"], {"a", "c"}, None, 5, ["c", "a"]),
        (["c", "b", "a"], {"a", "b", "c"}, "b", 5, ["c", "a"]),
        (["a", "b", "a", "c", "b"], {"a", "b", "c"}, None, 5, ["a", "b", "c"]),
        (["e", "d", "c", "b", "a"], {"a", "b", "c", "d", "e"}, None, 3, ["e", "d", "c"]),
    ],
)
def test_compute_visible_recents(recents, online, current, limit, expected):
    assert compute_visible_recents(
        recents,
        online,
        current=current,
        limit=limit,
    ) == expected


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


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        (f"{CURRENT_MARK} torale", ("select", "torale")),
        (f"{CURRENT_OFF_MARK} torale", ("select", "torale")),
        (f"{RECENT_MARK} orch", ("select", "orch")),
        (PEERS_LABEL, ("peers", None)),
        (CLEAR_LABEL, ("clear", None)),
        (MORE_LABEL, ("more", None)),
        ("hello there", ("text", None)),
        ("@torale do it", ("text", None)),
        (CURRENT_MARK, ("text", None)),
        (f"{CURRENT_MARK} ", ("text", None)),
    ],
)
def test_parse_keyboard_tap(text, expected):
    assert parse_keyboard_tap(text) == expected


# -- explicit notify/FYI command parsing --


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("/notify @agent build passed", ("agent", "build passed")),
        ("/fyi build passed", (None, "build passed")),
        ("notify @agent build passed", None),
    ],
)
def test_parse_notify_command(text, expected):
    assert parse_notify_command(text) == expected


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
async def test_fetch_online_peers_hides_telegram_self(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    telegram_bot._display_name = "telegram-claude-code"
    telegram_bot._peer_id = "svc-telegram"
    get = AsyncMock(return_value=SimpleNamespace(json=lambda: {
        "peers": [
            {
                "peer_id": "svc-telegram",
                "display_name": "telegram-claude-code",
                "status": "online",
                "role": "service",
                "path": "/telegram",
            },
            {
                "peer_id": "agent-1",
                "display_name": "agent",
                "status": "online",
                "role": "agent",
                "path": "/projects/agent",
            },
        ],
    }))
    monkeypatch.setattr(telegram_bot._http, "get", get)

    peers = await telegram_bot._fetch_online_peers(use_cache=False)

    assert [p["display_name"] for p in peers] == ["agent"]


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


@pytest.mark.asyncio
async def test_inline_target_callback_replays_pending_retry(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    post = AsyncMock()
    monkeypatch.setattr(telegram_bot._http, "post", post)
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_send_peer_message", send)
    telegram_bot._pending_retry = PendingRetry(
        text="please retry",
        expires_at=time.monotonic() + 10,
        mode="ask",
        attachments=[{"id": "att-1"}],
    )

    await telegram_bot._on_callback({
        "id": "cb-1",
        "data": "target:repow-default-orchestrator",
    })

    send.assert_awaited_once_with(
        "repow-default-orchestrator",
        "please retry",
        message_id=None,
        mode="ask",
        attachments=[{"id": "att-1"}],
    )
    assert telegram_bot._reply_target == "repow-default-orchestrator"
    assert telegram_bot._pending_retry is None


@pytest.mark.asyncio
async def test_current_keyboard_treats_peer_id_target_as_online(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    telegram_bot._reply_target = "repow-default-orchestrator"
    monkeypatch.setattr(
        telegram_bot,
        "_fetch_online_peers",
        AsyncMock(return_value=[
            {
                "peer_id": "repow-default-orchestrator",
                "display_name": "orchestrator-codex",
                "status": "online",
                "circle": "default",
                "path": "/orchestrator",
            },
        ]),
    )

    kb = await telegram_bot._current_reply_keyboard()

    assert kb["keyboard"][0][0]["text"] == f"{CURRENT_MARK} repow-default-orchestrator"
    assert kb["input_field_placeholder"] == "msg @repow-default-orchestrator..."


# -- PendingRetry TTL --


@pytest.mark.parametrize(
    ("offset", "expected"),
    [(0, True), (5, True), (10, False), (11, False)],
)
def test_pending_retry_ttl(offset, expected):
    now = time.monotonic()
    r = PendingRetry(text="hi", expires_at=now + 10)
    assert r.is_active(now + offset) is expected


@pytest.mark.asyncio
async def test_choice_question_renders_option_buttons(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_tg_send", send)
    monkeypatch.setattr(telegram_bot, "_tg_send_attachments", AsyncMock())

    await telegram_bot._on_ws({
        "type": "ask",
        "from_peer": "agent",
        "correlation_id": "ask-abc123",
        "text": "run rm -rf?",
        "question": {
            "kind": "choice",
            "options": [
                {"id": "allow", "title": "Allow"},
                {"id": "deny", "title": "Deny"},
            ],
        },
    })

    markup = send.await_args.kwargs["markup"]
    buttons = [b for row in markup["inline_keyboard"] for b in row]
    assert [b["text"] for b in buttons] == ["Allow", "Deny"]
    assert [b["callback_data"] for b in buttons] == [
        "answer:ask-abc123:0", "answer:ask-abc123:1",
    ]
    # the option ids are remembered for the index-based callback
    assert telegram_bot._question_options["ask-abc123"] == ["allow", "deny"]


@pytest.mark.asyncio
async def test_plain_ask_renders_ack_button(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_tg_send", send)
    monkeypatch.setattr(telegram_bot, "_tg_send_attachments", AsyncMock())

    await telegram_bot._on_ws({
        "type": "ask", "from_peer": "agent",
        "correlation_id": "ask-xyz", "text": "fyi",
    })
    markup = send.await_args.kwargs["markup"]
    buttons = [b for row in markup["inline_keyboard"] for b in row]
    assert [b["callback_data"] for b in buttons] == ["ack:ask-xyz"]


@pytest.mark.asyncio
async def test_answer_callback_posts_chosen_option(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    telegram_bot._question_options["ask-abc123"] = ["allow", "deny"]
    post = AsyncMock(return_value=SimpleNamespace(status_code=200))
    monkeypatch.setattr(telegram_bot._http, "post", post)
    monkeypatch.setattr(telegram_bot, "_tg_send", AsyncMock())

    await telegram_bot._on_callback({"id": "cb-1", "data": "answer:ask-abc123:1"})

    # the /answer POST carried the real option_id resolved from index 1
    answer_call = next(
        c for c in post.await_args_list if str(c.args[0]).endswith("/answer")
    )
    assert answer_call.kwargs["json"] == {
        "correlation_id": "ask-abc123", "option_id": "deny",
    }
    # cleared after a successful answer
    assert "ask-abc123" not in telegram_bot._question_options


@pytest.mark.asyncio
async def test_question_options_map_is_bounded(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # The cid->option-ids map must not grow unbounded over a long bot process.
    from repowire.telegram.bot import _MAX_REMEMBERED_QUESTIONS
    monkeypatch.setattr(telegram_bot, "_tg_send", AsyncMock())
    monkeypatch.setattr(telegram_bot, "_tg_send_attachments", AsyncMock())
    q = {"kind": "choice", "options": [{"id": "a", "title": "A"}]}
    for i in range(_MAX_REMEMBERED_QUESTIONS + 50):
        await telegram_bot._on_ws({
            "type": "ask", "from_peer": "agent",
            "correlation_id": f"ask-{i}", "text": "?", "question": q,
        })
    assert len(telegram_bot._question_options) <= _MAX_REMEMBERED_QUESTIONS


@pytest.mark.asyncio
async def test_stale_option_index_clears_entry(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    telegram_bot._question_options["ask-1"] = ["allow", "deny"]
    monkeypatch.setattr(telegram_bot._http, "post", AsyncMock(
        return_value=SimpleNamespace(status_code=200),
    ))
    monkeypatch.setattr(telegram_bot, "_tg_send", AsyncMock())
    await telegram_bot._on_callback({"id": "cb", "data": "answer:ask-1:9"})  # out of range
    assert "ask-1" not in telegram_bot._question_options


@pytest.mark.asyncio
async def test_tool_permission_question_renders_deny_button(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # ACP tool-permission options are allow-only → an explicit Deny must appear.
    send = AsyncMock()
    monkeypatch.setattr(telegram_bot, "_tg_send", send)
    monkeypatch.setattr(telegram_bot, "_tg_send_attachments", AsyncMock())
    await telegram_bot._on_ws({
        "type": "ask", "from_peer": "worker", "correlation_id": "acpperm-1", "text": "Allow shell?",
        "question": {"kind": "choice", "scope": "tool_permission",
                     "options": [{"id": "allow", "title": "Allow"}]},
    })
    markup = send.await_args.kwargs["markup"]
    buttons = [b for row in markup["inline_keyboard"] for b in row]
    assert [b["callback_data"] for b in buttons] == ["answer:acpperm-1:0", "deny:acpperm-1"]


@pytest.mark.asyncio
async def test_deny_callback_posts_denied_outcome(
    telegram_bot: TelegramPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    post = AsyncMock(return_value=SimpleNamespace(status_code=200))
    monkeypatch.setattr(telegram_bot._http, "post", post)
    monkeypatch.setattr(telegram_bot, "_tg_send", AsyncMock())
    await telegram_bot._on_callback({"id": "cb", "data": "deny:acpperm-1"})
    call = next(c for c in post.await_args_list if str(c.args[0]).endswith("/answer"))
    assert call.kwargs["json"] == {"correlation_id": "acpperm-1", "outcome": "denied"}
