"""FastAPI dependencies for the Repowire daemon."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from repowire.config.models import Config, load_config
from repowire.daemon.core import PeerManager

if TYPE_CHECKING:
    from repowire.backends.base import Backend


# Global state - initialized by lifespan
_config: Config | None = None
_backend: Any | None = None  # May be None in per-peer routing mode
_peer_manager: PeerManager | None = None
_app_state: Any | None = None  # FastAPI app.state


def init_deps(
    config: Config, backend: Any | None, peer_manager: PeerManager, app_state: Any | None = None
) -> None:
    """Initialize dependencies. Called by app lifespan.

    Args:
        config: Configuration instance
        backend: Backend instance (may be None in per-peer routing mode)
        peer_manager: PeerManager instance
        app_state: FastAPI app.state instance
    """
    global _config, _backend, _peer_manager, _app_state
    _config = config
    _backend = backend
    _peer_manager = peer_manager
    _app_state = app_state


def cleanup_deps() -> None:
    """Cleanup dependencies. Called by app lifespan."""
    global _config, _backend, _peer_manager, _app_state
    _config = None
    _backend = None
    _peer_manager = None
    _app_state = None


def get_config() -> Config:
    """Get the current configuration."""
    if _config is None:
        # Fallback to loading from disk
        return load_config()
    return _config


def get_backend() -> Backend | None:
    """Get the message delivery backend.

    Returns None in per-peer routing mode where there's no single backend.
    Use peer_manager methods for actual message routing.
    """
    return _backend


def get_peer_manager() -> PeerManager:
    """Get the peer manager instance."""
    if _peer_manager is None:
        raise RuntimeError("PeerManager not initialized. Is the daemon running?")
    return _peer_manager


def get_app_state() -> Any:
    """Get the FastAPI app.state instance."""
    if _app_state is None:
        raise RuntimeError("App state not initialized. Is the daemon running?")
    return _app_state
