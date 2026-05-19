"""ACP subprocess transport.

Thin wrapper around `acp.spawn_agent_process` that owns the subprocess and the
JSON-RPC connection lifecycle. The high-level `AcpClient` in `client.py` drives
the connection; this module exists to keep subprocess setup and teardown
isolated from the broker-side wiring.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator, Mapping
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import acp
    from acp.client.connection import ClientSideConnection

logger = logging.getLogger(__name__)


@dataclass
class AcpSubprocess:
    """Bundle of a live ACP connection and the subprocess backing it."""

    connection: ClientSideConnection
    process: asyncio.subprocess.Process


@asynccontextmanager
async def spawn_acp_subprocess(
    client: acp.Client,
    command: str,
    *args: str,
    cwd: str | Path | None = None,
    env: Mapping[str, str] | None = None,
) -> AsyncIterator[AcpSubprocess]:
    """Spawn an ACP agent subprocess and yield a live connection bound to ``client``.

    The SDK's ``spawn_agent_process`` already handles stdio framing and JSON-RPC
    routing; we just record the process handle so the broker can correlate
    crashes with peer state and so tests can introspect lifecycle.
    """
    import acp

    async with acp.spawn_agent_process(
        client,
        command,
        *args,
        cwd=cwd,
        env=dict(env) if env else None,
    ) as (connection, process):
        logger.info(
            "ACP subprocess up: pid=%s command=%s cwd=%s",
            getattr(process, "pid", None),
            command,
            cwd,
        )
        try:
            yield AcpSubprocess(connection=connection, process=process)
        finally:
            logger.info(
                "ACP subprocess down: pid=%s exit=%s",
                getattr(process, "pid", None),
                process.returncode,
            )
