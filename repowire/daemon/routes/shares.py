"""Share session endpoints — wraps relay share-token creation.

POST /shares          create a share token via the relay, return {share_id, url, ...}
GET  /shares          list active share tokens for this daemon's relay user
DELETE /shares/{id}   revoke a share token via the relay
"""

from __future__ import annotations

import logging

import httpx
from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state

log = logging.getLogger(__name__)
router = APIRouter(tags=["shares"])


class ShareRequest(BaseModel):
    peer_name: str = Field(..., description="Display name of the peer to share")
    permissions: str = Field("ro", description="'ro' (read-only) or 'rw' (read-write)")
    ttl_secs: int | None = Field(None, description="Token lifetime in seconds; None = no expiry")


def _relay_http_and_key() -> tuple[str, str] | None:
    """Return (relay_http_base, api_key) if relay is configured, else None."""
    state = get_app_state()
    cfg = state.config  # type: ignore[attr-defined]
    relay = cfg.relay
    if not relay.enabled or not relay.api_key:
        return None
    base = relay.url.replace("wss://", "https://").replace("ws://", "http://").rstrip("/")
    return base, relay.api_key


@router.post("/shares")
async def create_share(
    req: ShareRequest,
    _: str | None = Depends(require_auth),
) -> dict:
    cfg = _relay_http_and_key()
    if cfg is None:
        raise HTTPException(
            status_code=503,
            detail="Relay not configured. Run `repowire setup --relay` first.",
        )
    base, api_key = cfg
    async with httpx.AsyncClient(timeout=10.0) as client:
        resp = await client.post(
            f"{base}/api/v1/share",
            json={
                "peer_name": req.peer_name,
                "permissions": req.permissions,
                "ttl_secs": req.ttl_secs,
            },
            headers={"x-api-key": api_key},
        )
    if resp.status_code != 200:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    data = resp.json()
    data["url"] = f"{base}/s/{data['share_id']}"
    return data


@router.get("/shares")
async def list_shares(
    _: str | None = Depends(require_auth),
) -> list[dict]:
    cfg = _relay_http_and_key()
    if cfg is None:
        return []
    base, api_key = cfg
    async with httpx.AsyncClient(timeout=10.0) as client:
        resp = await client.get(
            f"{base}/api/v1/share",
            headers={"x-api-key": api_key},
        )
    if resp.status_code != 200:
        return []
    tokens = resp.json()
    for t in tokens:
        t["url"] = f"{base}/s/{t['share_id']}"
    return tokens


@router.delete("/shares/{share_id}")
async def revoke_share(
    share_id: str,
    _: str | None = Depends(require_auth),
) -> dict:
    cfg = _relay_http_and_key()
    if cfg is None:
        raise HTTPException(status_code=503, detail="Relay not configured")
    base, api_key = cfg
    async with httpx.AsyncClient(timeout=10.0) as client:
        resp = await client.delete(
            f"{base}/api/v1/share/{share_id}",
            headers={"x-api-key": api_key},
        )
    if resp.status_code not in (200, 404):
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    return resp.json()
