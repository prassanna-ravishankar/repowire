"""Authentication middleware for the Repowire daemon."""

from __future__ import annotations

from fastapi import Depends, HTTPException, Request, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from repowire.daemon.deps import get_config

# Optional bearer token auth
_bearer_scheme = HTTPBearer(auto_error=False)


async def verify_api_key(
    request: Request,
    credentials: HTTPAuthorizationCredentials | None = Depends(_bearer_scheme),
) -> str | None:
    """Verify API key for relay mode.

    Returns the API key if valid, None if auth is disabled.
    Raises HTTPException if auth is required but invalid.
    """
    config = get_config()

    # Only enforce auth if daemon.auth_token is set (local daemon auth)
    # relay.api_key is for connecting TO the relay, not for local endpoints
    if not config.daemon.auth_token:
        return None

    if credentials is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authorization header",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if credentials.credentials != config.daemon.auth_token:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid auth token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return credentials.credentials


class RequireAuth:
    """Dependency that requires authentication when relay mode is enabled.

    TODO(relay): currently auth is only enforced in relay mode. When going
    hosted, enforce auth on all endpoints (spawn, messages, peers) regardless
    of relay flag. Grep for TODO(relay) to find all related sites.
    """

    async def __call__(
        self,
        api_key: str | None = Depends(verify_api_key),
    ) -> str | None:
        """Return the API key (or None if auth disabled)."""
        return api_key


# Convenience instance
require_auth = RequireAuth()


async def require_localhost(request: Request) -> None:
    """Require request originates from localhost."""
    client_host = request.client.host if request.client else None
    if client_host not in ("127.0.0.1", "::1", "localhost", None):
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="Restricted to localhost",
        )
