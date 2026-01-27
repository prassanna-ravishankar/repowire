"""Tests for SSE stream service."""

from __future__ import annotations

import asyncio
import json
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import httpx

from repowire.tui.services.sse_stream import SSEStream


class TestSSEStreamInit:
    """Tests for SSEStream initialization."""

    def test_init_default_url(self) -> None:
        """Test default URL is set."""
        stream = SSEStream()
        assert stream.base_url == "http://127.0.0.1:8377"

    def test_init_custom_url(self) -> None:
        """Test custom URL is used."""
        stream = SSEStream("http://localhost:9000")
        assert stream.base_url == "http://localhost:9000"

    def test_init_running_false(self) -> None:
        """Test _running is False initially."""
        stream = SSEStream()
        assert stream._running is False


class TestSSEStreamStop:
    """Tests for SSEStream.stop method."""

    def test_stop_sets_running_false(self) -> None:
        """Test stop sets _running to False."""
        stream = SSEStream()
        stream._running = True
        stream.stop()
        assert stream._running is False

    def test_stop_when_already_stopped(self) -> None:
        """Test stop is idempotent."""
        stream = SSEStream()
        stream._running = False
        stream.stop()
        assert stream._running is False


def _create_mock_stream_context(aiter_lines_fn: Any) -> MagicMock:
    """Helper to create properly nested mock for httpx streaming."""
    # Response mock with async iterator
    mock_response = MagicMock()
    mock_response.aiter_lines = aiter_lines_fn

    # Inner context manager for client.stream()
    mock_stream_ctx = MagicMock()
    mock_stream_ctx.__aenter__ = AsyncMock(return_value=mock_response)
    mock_stream_ctx.__aexit__ = AsyncMock(return_value=None)

    # Client mock
    mock_client = MagicMock()
    mock_client.stream = MagicMock(return_value=mock_stream_ctx)

    # Outer context manager for httpx.AsyncClient()
    mock_client_ctx = MagicMock()
    mock_client_ctx.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client_ctx.__aexit__ = AsyncMock(return_value=None)

    return mock_client_ctx


class TestSSEStreamEvents:
    """Tests for SSEStream.stream_events method."""

    async def test_stream_sets_running_true(self) -> None:
        """Test stream_events sets _running to True on start."""
        stream = SSEStream()
        running_during_stream = False

        async def mock_aiter_lines() -> Any:
            nonlocal running_during_stream
            running_during_stream = stream._running
            stream.stop()
            return
            yield  # Make it a generator

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for _ in stream.stream_events():
                pass

        assert running_during_stream is True

    async def test_stream_yields_parsed_events(self) -> None:
        """Test stream_events yields parsed JSON events."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines() -> Any:
            yield 'data: {"type": "test", "id": "1"}'
            yield 'data: {"type": "test", "id": "2"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                if len(events_received) >= 2:
                    stream.stop()
                    break

        assert len(events_received) == 2
        assert events_received[0] == {"type": "test", "id": "1"}
        assert events_received[1] == {"type": "test", "id": "2"}

    async def test_stream_ignores_non_data_lines(self) -> None:
        """Test stream ignores lines not starting with 'data: '."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines() -> Any:
            yield ": heartbeat"  # Comment line
            yield "event: test"  # Event type line
            yield ""  # Empty line
            yield 'data: {"type": "valid", "id": "1"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                stream.stop()
                break

        assert len(events_received) == 1
        assert events_received[0] == {"type": "valid", "id": "1"}

    async def test_stream_handles_malformed_json(self) -> None:
        """Test stream continues on malformed JSON."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines() -> Any:
            yield "data: {not valid json}"
            yield "data: {also invalid"
            yield 'data: {"type": "valid", "id": "1"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                stream.stop()
                break

        # Should have skipped the malformed ones and got the valid one
        assert len(events_received) == 1
        assert events_received[0] == {"type": "valid", "id": "1"}

    async def test_stream_reconnects_on_request_error(self) -> None:
        """Test stream reconnects on RequestError."""
        stream = SSEStream()
        call_count = 0
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines_with_error() -> Any:
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                raise httpx.RequestError("Connection lost")
            yield 'data: {"type": "reconnected", "id": "1"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines_with_error)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            with patch(
                "repowire.tui.services.sse_stream.asyncio.sleep", new_callable=AsyncMock
            ) as mock_sleep:
                async for event in stream.stream_events():
                    events_received.append(event)
                    stream.stop()
                    break

                # Sleep should have been called for reconnect delay
                mock_sleep.assert_called_once_with(2)

        assert len(events_received) == 1
        assert events_received[0] == {"type": "reconnected", "id": "1"}

    async def test_stream_stops_on_cancelled_error(self) -> None:
        """Test stream breaks on CancelledError."""
        stream = SSEStream()

        async def mock_aiter_lines() -> Any:
            raise asyncio.CancelledError()
            yield  # Make it a generator

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        events_received: list[dict[str, Any]] = []

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)

        assert len(events_received) == 0

    async def test_stream_correct_url(self) -> None:
        """Test stream uses correct URL."""
        stream = SSEStream("http://custom:9000")
        captured_url = None

        async def mock_aiter_lines() -> Any:
            stream.stop()
            return
            yield

        mock_response = MagicMock()
        mock_response.aiter_lines = mock_aiter_lines

        mock_stream_ctx = MagicMock()
        mock_stream_ctx.__aenter__ = AsyncMock(return_value=mock_response)
        mock_stream_ctx.__aexit__ = AsyncMock(return_value=None)

        def capture_stream(method: str, url: str) -> MagicMock:
            nonlocal captured_url
            captured_url = url
            return mock_stream_ctx

        mock_client = MagicMock()
        mock_client.stream = capture_stream

        mock_client_ctx = MagicMock()
        mock_client_ctx.__aenter__ = AsyncMock(return_value=mock_client)
        mock_client_ctx.__aexit__ = AsyncMock(return_value=None)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for _ in stream.stream_events():
                pass

        assert captured_url == "http://custom:9000/events/stream"

    async def test_stream_stops_when_running_false(self) -> None:
        """Test stream stops mid-iteration when _running becomes False."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines() -> Any:
            yield 'data: {"type": "test", "id": "1"}'
            stream.stop()  # Stop before second event
            yield 'data: {"type": "test", "id": "2"}'  # Should not be yielded

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)

        assert len(events_received) == 1
        assert events_received[0] == {"type": "test", "id": "1"}

    async def test_stream_handles_empty_data(self) -> None:
        """Test stream handles 'data: ' with empty payload."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        async def mock_aiter_lines() -> Any:
            yield "data: "  # Empty data
            yield 'data: {"type": "valid"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                stream.stop()
                break

        # Empty data should fail JSON parse and be skipped
        assert len(events_received) == 1
        assert events_received[0] == {"type": "valid"}

    async def test_stream_complex_json(self) -> None:
        """Test stream handles complex JSON payloads."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        complex_event = {
            "type": "query",
            "id": "abc-123",
            "from_peer": "frontend",
            "to_peer": "backend",
            "payload": {"message": "Hello, world!", "metadata": {"key": "value"}},
            "timestamp": "2024-01-15T10:30:00Z",
        }

        async def mock_aiter_lines() -> Any:
            yield f"data: {json.dumps(complex_event)}"
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                stream.stop()
                break

        assert len(events_received) == 1
        assert events_received[0] == complex_event

    async def test_stream_handles_nested_data_in_json(self) -> None:
        """Test stream handles JSON with 'data:' in the content."""
        stream = SSEStream()
        events_received: list[dict[str, Any]] = []

        event_with_data = {
            "type": "message",
            "content": "data: inside the payload",
        }

        async def mock_aiter_lines() -> Any:
            yield f"data: {json.dumps(event_with_data)}"
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            async for event in stream.stream_events():
                events_received.append(event)
                stream.stop()
                break

        assert len(events_received) == 1
        assert events_received[0] == event_with_data

    async def test_stream_while_loop_continues_on_error(self) -> None:
        """Test the while loop continues reconnecting after errors."""
        stream = SSEStream()
        error_count = 0
        success_count = 0

        async def mock_aiter_lines() -> Any:
            nonlocal error_count, success_count
            if error_count < 2:
                error_count += 1
                raise httpx.RequestError("Connection error")
            success_count += 1
            yield 'data: {"type": "success"}'
            stream.stop()

        mock_client_ctx = _create_mock_stream_context(mock_aiter_lines)

        events_received: list[dict[str, Any]] = []

        with patch(
            "repowire.tui.services.sse_stream.httpx.AsyncClient", return_value=mock_client_ctx
        ):
            with patch("repowire.tui.services.sse_stream.asyncio.sleep", new_callable=AsyncMock):
                async for event in stream.stream_events():
                    events_received.append(event)
                    stream.stop()
                    break

        assert error_count == 2  # Should have errored twice
        assert success_count == 1  # Then succeeded
        assert len(events_received) == 1
