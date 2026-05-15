"""Health check endpoint."""

from __future__ import annotations

from fastapi import APIRouter, Request
from pydantic import BaseModel

from repowire import __version__

router = APIRouter(tags=["health"])


class HealthResponse(BaseModel):
    """Health check response."""

    status: str
    version: str
    relay_mode: bool = False


@router.get("/health", response_model=HealthResponse)
async def health_check(request: Request) -> HealthResponse:
    """Check daemon health status."""
    relay_mode = getattr(request.app.state, "relay_mode", False)

    return HealthResponse(
        status="ok",
        version=__version__,
        relay_mode=relay_mode,
    )
