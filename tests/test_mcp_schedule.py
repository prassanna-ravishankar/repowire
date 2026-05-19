from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from repowire.mcp.server import create_mcp_server


def _tool(name: str):
    return create_mcp_server()._tool_manager._tools[name].fn


@pytest.mark.asyncio
async def test_schedule_list_preserves_default_tsv_schema() -> None:
    with (
        patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock),
        patch("repowire.mcp.server._get_my_peer_name", new_callable=AsyncMock) as get_name,
        patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock) as request,
    ):
        get_name.return_value = "me"
        request.return_value = {"schedules": [{
            "schedule_id": "sched-1",
            "from_peer": "me",
            "to_peer": "me",
            "kind": "notify",
            "fire_at": "2026-05-19T09:00:00+00:00",
            "cron": "*/15 * * * *",
            "text": "stretch",
        }]}

        result = await _tool("schedule_list")()

    lines = result.splitlines()
    assert lines[0] == "schedule_id\tfrom_peer\tto_peer\tkind\tfire_at\ttext"
    assert lines[1].split("\t") == [
        "sched-1", "me", "me", "notify",
        "2026-05-19T09:00:00+00:00", "stretch",
    ]


@pytest.mark.asyncio
async def test_schedule_list_can_opt_into_cron_column() -> None:
    with (
        patch("repowire.mcp.server._ensure_registered", new_callable=AsyncMock),
        patch("repowire.mcp.server._get_my_peer_name", new_callable=AsyncMock) as get_name,
        patch("repowire.mcp.server.daemon_request", new_callable=AsyncMock) as request,
    ):
        get_name.return_value = "me"
        request.return_value = {"schedules": [{
            "schedule_id": "sched-1",
            "from_peer": "me",
            "to_peer": "me",
            "kind": "notify",
            "fire_at": "2026-05-19T09:00:00+00:00",
            "cron": "*/15 * * * *",
            "text": "stretch",
        }]}

        result = await _tool("schedule_list")(include_cron=True)

    lines = result.splitlines()
    assert lines[0] == "schedule_id\tfrom_peer\tto_peer\tkind\tfire_at\ttext\tcron"
    assert lines[1].split("\t") == [
        "sched-1", "me", "me", "notify",
        "2026-05-19T09:00:00+00:00", "stretch", "*/15 * * * *",
    ]
