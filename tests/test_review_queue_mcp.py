"""Tests for the mark_reviewed / review_queue MCP tools."""

from __future__ import annotations

from unittest.mock import patch

import pytest

from repowire.mcp import server as mcp_server


@pytest.fixture(autouse=True)
def _reset_mcp_state():
    mcp_server._cached_peer_name = "alice"
    mcp_server._cached_peer_id = None
    mcp_server._registered = True
    yield
    mcp_server._cached_peer_name = None
    mcp_server._cached_peer_id = None
    mcp_server._registered = False


def _text_from_result(result) -> str:
    """FastMCP call_tool returns a list of content blocks."""
    if hasattr(result, "content"):
        result = result.content
    if isinstance(result, list):
        for block in result:
            if hasattr(block, "text"):
                return block.text
            if isinstance(block, dict) and "text" in block:
                return block["text"]
    if isinstance(result, tuple) and result:
        first = result[0]
        if isinstance(first, list):
            for block in first:
                if hasattr(block, "text"):
                    return block.text
    return str(result)


@pytest.mark.asyncio
async def test_mark_reviewed_posts_to_daemon():
    mcp = mcp_server.create_mcp_server()
    captured: dict = {}

    async def fake_request(method, path, body=None, params=None):
        captured["method"] = method
        captured["path"] = path
        captured["body"] = body
        return {"ok": True}

    with patch.object(mcp_server, "daemon_request", side_effect=fake_request):
        result = await mcp.call_tool(
            "mark_reviewed",
            {"pr_url": "https://github.com/o/r/pull/1", "last_reviewed_sha": "abc"},
        )

    assert captured["method"] == "POST"
    assert captured["path"] == "/reviews"
    assert captured["body"] == {
        "reviewer": "alice",
        "pr_url": "https://github.com/o/r/pull/1",
        "last_reviewed_sha": "abc",
    }
    assert "marked reviewed" in _text_from_result(result)


@pytest.mark.asyncio
async def test_mark_reviewed_without_sha_omits_field():
    mcp = mcp_server.create_mcp_server()
    captured: dict = {}

    async def fake_request(method, path, body=None, params=None):
        captured["body"] = body
        return {"ok": True}

    with patch.object(mcp_server, "daemon_request", side_effect=fake_request):
        await mcp.call_tool(
            "mark_reviewed", {"pr_url": "https://github.com/o/r/pull/1"},
        )

    assert "last_reviewed_sha" not in captured["body"]


@pytest.mark.asyncio
async def test_review_queue_returns_tsv():
    mcp = mcp_server.create_mcp_server()

    async def fake_request(method, path, body=None, params=None):
        assert path == "/reviews"
        assert params == {"reviewer": "alice"}
        return {
            "reviews": [
                {
                    "pr_url": "https://github.com/o/r/pull/1",
                    "last_reviewed_sha": "abc",
                    "current_head_sha": "abc",
                    "state": "open",
                    "my_action": "none-needed",
                },
                {
                    "pr_url": "https://github.com/o/r/pull/2",
                    "last_reviewed_sha": "old",
                    "current_head_sha": "new",
                    "state": "open",
                    "my_action": "re-review-suggested",
                },
            ]
        }

    with patch.object(mcp_server, "daemon_request", side_effect=fake_request):
        result = await mcp.call_tool("review_queue", {})

    tsv = _text_from_result(result)
    lines = tsv.strip().splitlines()
    assert lines[0].split("\t") == [
        "pr_url",
        "last_reviewed_sha",
        "current_head_sha",
        "state",
        "my_action",
    ]
    assert "none-needed" in lines[1]
    assert "re-review-suggested" in lines[2]


@pytest.mark.asyncio
async def test_review_queue_defaults_to_caller():
    mcp = mcp_server.create_mcp_server()
    seen: dict = {}

    async def fake_request(method, path, body=None, params=None):
        seen["params"] = params
        return {"reviews": []}

    with patch.object(mcp_server, "daemon_request", side_effect=fake_request):
        await mcp.call_tool("review_queue", {})

    assert seen["params"]["reviewer"] == "alice"


@pytest.mark.asyncio
async def test_review_queue_explicit_peer_name():
    mcp = mcp_server.create_mcp_server()
    seen: dict = {}

    async def fake_request(method, path, body=None, params=None):
        seen["params"] = params
        return {"reviews": []}

    with patch.object(mcp_server, "daemon_request", side_effect=fake_request):
        await mcp.call_tool("review_queue", {"peer_name": "bob"})

    assert seen["params"]["reviewer"] == "bob"
