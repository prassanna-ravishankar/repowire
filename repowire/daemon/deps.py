"""FastAPI dependencies for the Repowire daemon."""

from __future__ import annotations

from typing import Any, Protocol, runtime_checkable

from repowire.config.models import Config, load_config
from repowire.daemon.core import PeerManager


@runtime_checkable
class AppState(Protocol):
    """Protocol for FastAPI app.state with known attributes."""

    session_mapper: Any
    transport: Any
    query_tracker: Any
    peer_manager: PeerManager
    config: Config


# Global state - initialized by lifespan
_config: Config | None = None
_peer_manager: PeerManager | None = None
_app_state: AppState | None = None


def init_deps(config: Config, peer_manager: PeerManager, app_state: AppState | None = None) -> None:
    """Initialize dependencies. Called by app lifespan.

    Args:
        config: Configuration instance
        peer_manager: PeerManager instance
        app_state: FastAPI app.state instance
    """
    global _config, _peer_manager, _app_state
    _config = config
    _peer_manager = peer_manager
    _app_state = app_state


def cleanup_deps() -> None:
    """Cleanup dependencies. Called by app lifespan."""
    global _config, _peer_manager, _app_state
    _config = None
    _peer_manager = None
    _app_state = None


def get_config() -> Config:
    """Get the current configuration."""
    if _config is None:
        return load_config()
    return _config


def get_peer_manager() -> PeerManager:
    """Get the peer manager instance."""
    if _peer_manager is None:
        raise RuntimeError("PeerManager not initialized. Is the daemon running?")
    return _peer_manager


def get_app_state() -> AppState:
    """Get the FastAPI app.state instance."""
    if _app_state is None:
        raise RuntimeError("App state not initialized. Is the daemon running?")
    return _app_state
