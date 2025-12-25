"""API key authentication for the relay server."""

from __future__ import annotations

import json
import secrets
from pathlib import Path
from datetime import datetime
from typing import Any

from pydantic import BaseModel, Field


API_KEYS_PATH = Path.home() / ".repowire" / "api_keys.json"
API_KEY_PREFIX = "rw_"
API_KEY_LENGTH = 32


class APIKey(BaseModel):
    """An API key for relay authentication."""

    key: str = Field(..., description="The API key")
    user_id: str = Field(..., description="User identifier")
    name: str = Field(default="default", description="Key name/label")
    created_at: datetime = Field(default_factory=datetime.utcnow)
    last_used: datetime | None = Field(default=None)


def _load_keys() -> dict[str, Any]:
    if not API_KEYS_PATH.exists():
        return {"keys": {}}
    return json.loads(API_KEYS_PATH.read_text())


def _save_keys(data: dict[str, Any]) -> None:
    API_KEYS_PATH.parent.mkdir(parents=True, exist_ok=True)
    API_KEYS_PATH.write_text(json.dumps(data, indent=2, default=str))


def generate_api_key(user_id: str, name: str = "default") -> APIKey:
    """Generate a new API key for a user."""
    key = f"{API_KEY_PREFIX}{secrets.token_urlsafe(API_KEY_LENGTH)}"
    api_key = APIKey(key=key, user_id=user_id, name=name)

    data = _load_keys()
    data["keys"][key] = api_key.model_dump(mode="json")
    _save_keys(data)

    return api_key


def validate_api_key(key: str) -> APIKey | None:
    """Validate an API key and return the APIKey if valid."""
    if not key.startswith(API_KEY_PREFIX):
        return None

    data = _load_keys()
    key_data = data["keys"].get(key)

    if not key_data:
        return None

    api_key = APIKey(**key_data)

    key_data["last_used"] = datetime.utcnow().isoformat()
    _save_keys(data)

    return api_key


def list_api_keys(user_id: str | None = None) -> list[APIKey]:
    """List all API keys, optionally filtered by user_id."""
    data = _load_keys()
    keys = [APIKey(**v) for v in data["keys"].values()]
    if user_id:
        keys = [k for k in keys if k.user_id == user_id]
    return keys


def revoke_api_key(key: str) -> bool:
    """Revoke an API key. Returns True if key was found and revoked."""
    data = _load_keys()
    if key in data["keys"]:
        del data["keys"][key]
        _save_keys(data)
        return True
    return False
