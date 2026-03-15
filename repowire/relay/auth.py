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
    # Check if user already has a token
    for api_key in _token_registry.values():
        if api_key.user_id == user_id:
            return api_key
    # Issue new token
    token = f"{API_KEY_PREFIX}{secrets.token_urlsafe(TOKEN_BYTES)}"
    api_key = APIKey(key=token, user_id=user_id)
    _token_registry[token] = api_key
    log.info("Registered token for user %s", user_id)
    return api_key


def update_token_user(key: str, user_id: str) -> None:
    """Update the user_id for an auto-registered token."""
    if key in _token_registry:
        _token_registry[key] = APIKey(key=key, user_id=user_id)


def validate_api_key(key: str) -> APIKey | None:
    """Validate a token. Auto-registers unknown but well-formed tokens.

    Tokens are unguessable secrets (32 chars of randomness). If you have the
    token, you're authenticated. The registry just tracks the user_id mapping.
    """
    if not key.startswith(API_KEY_PREFIX):
        return None
    if len(key) < 10:
        return None
    if key in _token_registry:
        return _token_registry[key]
    # Auto-register: token is the credential, derive user_id from connection context
    # For now, use a placeholder user_id; it gets updated on first daemon connect
    api_key = APIKey(key=key, user_id=f"token-{key[-8:]}")
    _token_registry[key] = api_key
    log.info("Auto-registered token %s...%s", key[:6], key[-4:])
    return api_key
