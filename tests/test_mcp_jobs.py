from __future__ import annotations

import json
from unittest.mock import AsyncMock, patch

import pytest

from repowire.mcp.server import create_mcp_server


def _tool(name: str):
    return create_mcp_server()._tool_manager._tools[name].fn


@pytest.mark.asyncio
async def test_job_create_posts_json_with_caller_peer_id() -> None:
    with (
        patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock),
        patch("repowire.mcp.server._get_my_peer_identifier", new_callable=AsyncMock) as peer,
        patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock) as request,
    ):
        peer.return_value = "repow-default-creator"
        request.return_value = {
            "job_id": "work-abc123",
            "work_id": "work-abc123",
            "status": {"state": "queued"},
        }

        result = await _tool("job_create")(
            title="Run checks",
            kind="verification",
            assigned_peer_id="repow-default-worker",
            request={"command": "pytest"},
        )

    request.assert_awaited_once()
    method, path, body = request.await_args.args[:3]
    assert method == "POST"
    assert path == "/jobs"
    assert body["created_by_peer_id"] == "repow-default-creator"
    assert body["assigned_peer_id"] == "repow-default-worker"
    assert body["request"] == {"command": "pytest"}
    assert json.loads(result)["job_id"] == "work-abc123"


@pytest.mark.asyncio
async def test_job_list_passes_filters_and_returns_json() -> None:
    with (
        patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock),
        patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock) as request,
    ):
        request.return_value = {"work": [{"job_id": "work-abc123"}]}

        result = await _tool("job_list")(state="running", circle="default")

    request.assert_awaited_once_with(
        "GET",
        "/jobs",
        params={"state": "running", "circle": "default"},
    )
    assert json.loads(result)["work"][0]["job_id"] == "work-abc123"


@pytest.mark.asyncio
async def test_job_status_show_result_update_and_cancel_wrap_jobs_api() -> None:
    calls: list[tuple] = []

    async def fake_request(method, path, body=None, params=None):
        calls.append((method, path, body, params))
        if path.endswith("/result"):
            return {"result": {"job_id": "work-abc123", "result_state": "not_ready"}}
        return {"status": {"job_id": "work-abc123", "state": "running"}}

    with (
        patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock),
        patch("repowire.mcp.server._get_my_peer_identifier", new_callable=AsyncMock) as peer,
        patch("repowire.mcp.server.daemon_request", side_effect=fake_request),
    ):
        peer.return_value = "repow-default-creator"
        await _tool("job_status")("work-abc123")
        await _tool("job_show")("work-abc123")
        await _tool("job_update")(
            "work-abc123",
            state="running",
            progress_note="started",
        )
        await _tool("job_result")("work-abc123")
        cancel_result = await _tool("job_cancel")("work-abc123", reason="stale")

    assert calls == [
        ("GET", "/jobs/work-abc123/status", None, None),
        ("GET", "/jobs/work-abc123/status", None, None),
        (
            "PATCH",
            "/jobs/work-abc123",
            {"state": "running", "progress_note": "started"},
            None,
        ),
        ("GET", "/jobs/work-abc123/result", None, None),
        (
            "POST",
            "/jobs/work-abc123/cancel",
            {"requested_by_peer_id": "repow-default-creator", "reason": "stale"},
            None,
        ),
    ]
    assert json.loads(cancel_result)["status"]["state"] == "running"
