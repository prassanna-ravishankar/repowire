"""Stateless HMAC-based API key authentication for the relay server."""

from __future__ import annotations

import hashlib
import hmac
import logging
import os

from pydantic import BaseModel, Field

log = logging.getLogger(__name__)

API_KEY_PREFIX = "rw_"
_DEV_SECRET = "repowire-dev-secret-do-not-use-in-production"


class APIKey(BaseModel):
    """An API key for relay authentication."""

    key: str = Field(..., description="The full API key string")
    user_id: str = Field(..., description="User identifier")
    name: str = Field(default="default", description="Key name/label")


_cached_secret: str | None = None
_cached_env_value: str | None = None


def _get_secret() -> str:
    """Return the HMAC signing secret from env, falling back to dev secret. Cached."""
    global _cached_secret, _cached_env_value
    env_value = os.environ.get("REPOWIRE_RELAY_SECRET")
    if _cached_secret is not None and env_value == _cached_env_value:
        return _cached_secret
    _cached_env_value = env_value
    if not env_value:
        log.warning("REPOWIRE_RELAY_SECRET not set — using insecure dev secret")
        _cached_secret = _DEV_SECRET
    else:
        _cached_secret = env_value
    return _cached_secret


def _compute_signature(secret: str, user_id: str) -> str:
    """HMAC-SHA256(secret, user_id), truncated to 16 hex chars."""
    sig = hmac.new(secret.encode(), user_id.encode(), hashlib.sha256).hexdigest()
    return sig[:16]


def generate_api_key(user_id: str, name: str = "default") -> APIKey:
    """Generate an API key: rw_{user_id}_{signature}."""
    secret = _get_secret()
    sig = _compute_signature(secret, user_id)
    key = f"{API_KEY_PREFIX}{user_id}_{sig}"
    return APIKey(key=key, user_id=user_id, name=name)


def validate_api_key(key: str) -> APIKey | None:
    """Parse and validate an API key by recomputing the HMAC signature."""
    if not key.startswith(API_KEY_PREFIX):
        return None

    body = key[len(API_KEY_PREFIX) :]
    parts = body.rsplit("_", 1)
    if len(parts) != 2:
        return None

    user_id, sig = parts
    if not user_id or not sig:
        return None

    secret = _get_secret()
    expected = _compute_signature(secret, user_id)
    if not hmac.compare_digest(sig, expected):
        return None

    return APIKey(key=key, user_id=user_id)
