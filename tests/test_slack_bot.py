"""Unit tests for Slack bot human inbound routing."""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest

from repowire.slack.bot import SlackPeer, parse_notify_command


@pytest.fixture
def slack_bot() -> SlackPeer:
    return SlackPeer(
        bot_token="xoxb-test",
        app_token="xapp-test",
        channel_id="C123",
        daemon_url="http://127.0.0.1:0",
        display_name="slack",
        circle="default",
    )


def test_parse_notify_command_with_peer() -> None:
    assert parse_notify_command("notify @agent build passed") == ("agent", "build passed")


def test_parse_fyi_command_without_peer_uses_sticky_target() -> None:
    assert parse_notify_command("fyi build passed") == (None, "build passed")


def test_parse_notify_command_ignores_slash_form() -> None:
    assert parse_notify_command("/notify @agent build passed") is None


@pytest.mark.asyncio
async def test_at_peer_text_opens_ask_by_default(
    slack_bot: SlackPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(slack_bot, "_ask", ask)
    monkeypatch.setattr(slack_bot, "_notify", notify)

    await slack_bot._on_text("@agent please check")

    ask.assert_awaited_once_with("agent", "please check")
    notify.assert_not_awaited()
    assert slack_bot._reply_target == "agent"


@pytest.mark.asyncio
async def test_sticky_text_opens_ask_by_default(
    slack_bot: SlackPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    slack_bot._reply_target = "agent"
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(slack_bot, "_ask", ask)
    monkeypatch.setattr(slack_bot, "_notify", notify)

    await slack_bot._on_text("please check")

    ask.assert_awaited_once_with("agent", "please check")
    notify.assert_not_awaited()


@pytest.mark.asyncio
async def test_notify_command_keeps_fire_and_forget(
    slack_bot: SlackPeer, monkeypatch: pytest.MonkeyPatch,
) -> None:
    ask = AsyncMock()
    notify = AsyncMock()
    monkeypatch.setattr(slack_bot, "_ask", ask)
    monkeypatch.setattr(slack_bot, "_notify", notify)

    await slack_bot._on_text("notify @agent build passed")

    notify.assert_awaited_once_with("agent", "build passed")
    ask.assert_not_awaited()
    assert slack_bot._reply_target == "agent"
