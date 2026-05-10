"""Tests for installers/post_spawn lifecycle middleware."""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from repowire.config.models import AgentType
from repowire.installers.post_spawn import (
    DEFAULT_WARMUP_TEMPLATE,
    _codex_warmup,
    post_spawn_warmup,
)


class TestPostSpawnWarmup:
    """post_spawn_warmup dispatches to the right backend handler."""

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_codex_calls_codex_warmup(self, mock_codex: AsyncMock) -> None:
        await post_spawn_warmup(
            AgentType.CODEX, "%42",
            path="/tmp/proj", circle="5", message=None,
        )
        mock_codex.assert_awaited_once()
        await_args = mock_codex.await_args
        assert await_args is not None
        pane_arg, text_arg = await_args.args
        assert pane_arg == "%42"
        assert "/tmp/proj" in text_arg
        assert "5" in text_arg

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_custom_message_overrides_default(self, mock_codex: AsyncMock) -> None:
        await post_spawn_warmup(
            AgentType.CODEX, "%42",
            path="/tmp/proj", circle="5",
            message="Custom task brief",
        )
        mock_codex.assert_awaited_once_with("%42", "Custom task brief")

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_claude_code_is_noop(self, mock_codex: AsyncMock) -> None:
        await post_spawn_warmup(
            AgentType.CLAUDE_CODE, "%42",
            path="/tmp/proj", circle="5", message=None,
        )
        mock_codex.assert_not_awaited()

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_opencode_is_noop(self, mock_codex: AsyncMock) -> None:
        await post_spawn_warmup(
            AgentType.OPENCODE, "%42",
            path="/tmp/proj", circle="5", message=None,
        )
        mock_codex.assert_not_awaited()

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_gemini_is_noop(self, mock_codex: AsyncMock) -> None:
        await post_spawn_warmup(
            AgentType.GEMINI, "%42",
            path="/tmp/proj", circle="5", message=None,
        )
        mock_codex.assert_not_awaited()

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._codex_warmup", new_callable=AsyncMock)
    async def test_warmup_swallows_exceptions(self, mock_codex: AsyncMock) -> None:
        """A stalled or buggy warmup must not bubble up to /spawn."""
        mock_codex.side_effect = RuntimeError("tmux exploded")
        # Must not raise.
        await post_spawn_warmup(
            AgentType.CODEX, "%42",
            path="/tmp/proj", circle="5", message=None,
        )


class TestCodexWarmup:
    """_codex_warmup drives the tmux send-keys sequence."""

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._tmux_send", new_callable=AsyncMock)
    @patch("repowire.installers.post_spawn.asyncio.sleep", new_callable=AsyncMock)
    @patch("repowire.installers.post_spawn.shutil.which", return_value="/usr/bin/tmux")
    async def test_codex_warmup_sequence(
        self,
        _mock_which,
        mock_sleep: AsyncMock,
        mock_send: AsyncMock,
    ) -> None:
        await _codex_warmup("%42", "Hello task")

        # 4 C-m for dialog dismissal + 1 message + 1 C-m submit = 6 sends
        assert mock_send.await_count == 6
        calls = mock_send.await_args_list
        # First 4 are C-m (dialog dismissals)
        for i in range(4):
            assert calls[i].args == ("%42", "C-m")
        # Then the literal message
        assert calls[4].args == ("%42", "Hello task")
        assert calls[4].kwargs == {"literal": True}
        # Then the final submit C-m
        assert calls[5].args == ("%42", "C-m")

        # Sleeps: 8s boot + 4×0.3s between C-m + 1s settle + 0.2s pre-submit
        sleep_args = [c.args[0] for c in mock_sleep.await_args_list]
        assert sleep_args[0] == 8  # boot
        assert sleep_args[1:5] == [0.3, 0.3, 0.3, 0.3]
        assert sleep_args[5] == 1  # settle
        assert sleep_args[6] == 0.2  # pre-submit

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._tmux_send", new_callable=AsyncMock)
    @patch("repowire.installers.post_spawn.asyncio.sleep", new_callable=AsyncMock)
    @patch("repowire.installers.post_spawn.shutil.which", return_value=None)
    async def test_codex_warmup_skips_when_tmux_missing(
        self,
        _mock_which,
        _mock_sleep: AsyncMock,
        mock_send: AsyncMock,
    ) -> None:
        await _codex_warmup("%42", "Hello task")
        mock_send.assert_not_awaited()

    @pytest.mark.asyncio
    @patch("repowire.installers.post_spawn._tmux_send", new_callable=AsyncMock)
    @patch("repowire.installers.post_spawn.asyncio.sleep", new_callable=AsyncMock)
    async def test_codex_warmup_skips_when_pane_id_empty(
        self,
        _mock_sleep: AsyncMock,
        mock_send: AsyncMock,
    ) -> None:
        await _codex_warmup("", "Hello task")
        mock_send.assert_not_awaited()


def test_default_warmup_template_renders() -> None:
    """The default template must accept path and circle placeholders."""
    rendered = DEFAULT_WARMUP_TEMPLATE.format(path="/tmp/proj", circle="5")
    assert "/tmp/proj" in rendered
    assert "5" in rendered
