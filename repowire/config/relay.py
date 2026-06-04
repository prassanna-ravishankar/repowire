"""Relay-related configuration models."""

from __future__ import annotations

from pydantic import BaseModel, Field


class RelayConfig(BaseModel):
    """Configuration for relay server connection."""

    enabled: bool = Field(default=False, description="Whether to connect to relay")
    url: str = Field(default="wss://repowire.io", description="Relay server URL")
    api_key: str | None = Field(None, description="API key for authentication")

    @property
    def dashboard_url(self) -> str | None:
        """Dashboard URL via the relay, or None if not configured."""
        if not self.api_key:
            return None
        return "https://repowire.io/dashboard"

    def ensure_api_key(self) -> str:
        """Register with relay and set API key if missing. Returns the key."""
        if self.api_key:
            return self.api_key
        import getpass

        import httpx

        relay_http = self.url.replace("wss://", "https://").replace("ws://", "http://")
        user_id = getpass.getuser()
        resp = httpx.post(
            f"{relay_http}/api/v1/register",
            json={"user_id": user_id},
            timeout=10.0,
        )
        resp.raise_for_status()
        self.api_key = resp.json()["api_key"]
        return self.api_key
