"""Token-based authentication for the relay server.

Tokens are server-issued random strings. The relay maintains an in-memory
registry of valid tokens. Clients obtain tokens via POST /api/v1/register.
"""

from __future__ import annotations

import logging
import secrets

from pydantic import BaseModel, Field

log = logging.getLogger(__name__)

API_KEY_PREFIX = "rw_"
TOKEN_BYTES = 24  # 32 chars base64url


class APIKey(BaseModel):
    """A relay API key (token)."""

    key: str = Field(..., description="The full API key string")
    user_id: str = Field(..., description="User identifier")


# Server-side token registry: key -> APIKey
_token_registry: dict[str, APIKey] = {}


def register_token(user_id: str) -> APIKey:
    """Issue a new token for a user. If user already has one, return it."""
    for api_key in _token_registry.values():
        if api_key.user_id == user_id:
            return api_key
    token = f"{API_KEY_PREFIX}{secrets.token_urlsafe(TOKEN_BYTES)}"
    api_key = APIKey(key=token, user_id=user_id)
    _token_registry[token] = api_key
    log.info("Registered token for user %s", user_id)
    return api_key


def validate_api_key(key: str) -> APIKey | None:
    """Validate a token against the registry."""
    return _token_registry.get(key)
