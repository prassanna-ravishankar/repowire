"""In-memory share token registry for session sharing.

Share tokens let a relay user hand someone a scoped URL that proxies
to a specific peer on their daemon — read-only (observe events) or
read-write (also inject asks).  Tokens live only for the relay process
lifetime; they are intentionally not persisted (restarting the relay
invalidates all share links).
"""

from __future__ import annotations

import logging
import secrets
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

log = logging.getLogger(__name__)

_PREFIX = "sh_"
_TOKEN_BYTES = 12  # 16 chars base64url


@dataclass
class ShareToken:
    share_id: str
    user_id: str
    peer_name: str
    permissions: str  # "ro" | "rw"
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    expires_at: datetime | None = None

    def is_expired(self) -> bool:
        if self.expires_at is None:
            return False
        return datetime.now(timezone.utc) >= self.expires_at

    def to_dict(self) -> dict:
        return {
            "share_id": self.share_id,
            "peer_name": self.peer_name,
            "permissions": self.permissions,
            "created_at": self.created_at.isoformat(),
            "expires_at": self.expires_at.isoformat() if self.expires_at else None,
        }


_registry: dict[str, ShareToken] = {}


def create_share_token(
    user_id: str,
    peer_name: str,
    permissions: str = "ro",
    ttl_secs: int | None = None,
) -> ShareToken:
    share_id = f"{_PREFIX}{secrets.token_urlsafe(_TOKEN_BYTES)}"
    expires_at = datetime.now(timezone.utc) + timedelta(seconds=ttl_secs) if ttl_secs else None
    token = ShareToken(
        share_id=share_id,
        user_id=user_id,
        peer_name=peer_name,
        permissions=permissions,
        expires_at=expires_at,
    )
    _registry[share_id] = token
    log.info("share token created: %s peer=%s perms=%s", share_id, peer_name, permissions)
    return token


def validate_share_token(share_id: str) -> ShareToken | None:
    token = _registry.get(share_id)
    if token is None:
        return None
    if token.is_expired():
        _registry.pop(share_id, None)
        log.info("share token expired: %s", share_id)
        return None
    return token


def revoke_share_token(share_id: str) -> bool:
    removed = _registry.pop(share_id, None) is not None
    if removed:
        log.info("share token revoked: %s", share_id)
    return removed


def list_share_tokens(user_id: str) -> list[ShareToken]:
    return [t for t in _registry.values() if t.user_id == user_id and not t.is_expired()]
