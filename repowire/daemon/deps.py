"""FastAPI dependencies for the Repowire daemon."""

from __future__ import annotations

from typing import Any, Protocol, runtime_checkable

from repowire.config.models import Config, load_config
from repowire.daemon.lifecycle_handler import LifecycleHandler
from repowire.daemon.peer_registry import PeerRegistry


@runtime_checkable
class AppState(Protocol):
    """Protocol for FastAPI app.state with known attributes."""

    transport: Any
    query_tracker: Any
    peer_registry: PeerRegistry
    config: Config


# Global state - initialized by lifespan
_config: Config | None = None
_peer_registry: PeerRegistry | None = None
_lifecycle_handler: LifecycleHandler | None = None
_app_state: AppState | None = None


def init_deps(
    config: Config,
    peer_registry: PeerRegistry,
    app_state: AppState | None = None,
    lifecycle_handler: LifecycleHandler | None = None,
) -> None:
    """Initialize dependencies. Called by app lifespan."""
    global _config, _peer_registry, _lifecycle_handler, _app_state
    _config = config
    _peer_registry = peer_registry
    _lifecycle_handler = lifecycle_handler
    _app_state = app_state


def cleanup_deps() -> None:
    """Cleanup dependencies. Called by app lifespan."""
    global _config, _peer_registry, _lifecycle_handler, _app_state
    _config = None
    _peer_registry = None
    _lifecycle_handler = None
    _app_state = None


def get_config() -> Config:
    """Get the current configuration."""
    if _config is None:
        return load_config()
    return _config


def get_peer_registry() -> PeerRegistry:
    """Get the peer registry instance."""
    if _peer_registry is None:
        raise RuntimeError("PeerRegistry not initialized. Is the daemon running?")
    return _peer_registry


# Backward compatibility alias
get_peer_manager = get_peer_registry


def get_lifecycle_handler() -> LifecycleHandler:
    """Get the lifecycle handler instance."""
    if _lifecycle_handler is None:
        raise RuntimeError("LifecycleHandler not initialized. Is the daemon running?")
    return _lifecycle_handler


def get_app_state() -> AppState:
    """Get the FastAPI app.state instance."""
    if _app_state is None:
        raise RuntimeError("App state not initialized. Is the daemon running?")
    return _app_state
