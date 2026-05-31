"""Read-only Beads board endpoint for the dashboard.

A short lazy cache coalesces the dashboard's polling into at most one ``bd``
read per TTL window — request-coalescing, not daemon polling. Read-only: the
underlying snapshot only runs ``bd ready`` / ``bd list``.
"""

from __future__ import annotations

import asyncio
import time

from fastapi import APIRouter, Depends

from repowire.daemon.auth import require_auth
from repowire.daemon.beads_board import BeadsBoard, read_beads_board

router = APIRouter(tags=["beads"])

# Coalesce dashboard polls; unavailable results cache briefly too, then retry.
_CACHE_TTL_SECONDS = 8.0
_cache: tuple[float, BeadsBoard] | None = None
_lock = asyncio.Lock()


@router.get("/beads/board")
async def beads_board(_: str | None = Depends(require_auth)) -> BeadsBoard:
    """Read-only snapshot of the repo's Beads board (ready/in-progress/blocked/closed).

    Cached for a few seconds so repeated dashboard refreshes don't each shell
    ``bd``. Always 200: when Beads is unavailable the body reports
    ``available=false`` with empty groups so the panel degrades quietly.
    """
    global _cache
    async with _lock:
        now = time.monotonic()
        if _cache is not None and now - _cache[0] < _CACHE_TTL_SECONDS:
            return _cache[1]
        board = await asyncio.to_thread(read_beads_board)
        _cache = (now, board)
        return board
