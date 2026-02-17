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

    # Skip auth if relay mode is disabled
    if not config.relay.enabled:
        return None

    # Skip auth if no API key is configured
    if not config.relay.api_key:
        return None

    # Auth is required - check credentials
    if credentials is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authorization header",
            headers={"WWW-Authenticate": "Bearer"},
        )

    # Verify the token format and value
    token = credentials.credentials
    if not token.startswith("rw_"):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid token format. Expected: Bearer rw_xxx",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if token != config.relay.api_key:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid API key",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return token


class RequireAuth:
    """Dependency that requires authentication when relay mode is enabled."""

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
