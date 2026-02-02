"""Backend implementations for message delivery."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from repowire.backends.base import Backend


def get_backend(name: str, **kwargs: Any) -> Backend:
    """Factory function to get a backend by name.

    Args:
        name: Backend name ("claudemux" or "opencode")
        **kwargs: Additional arguments passed to backend constructor
                  - query_tracker: QueryTracker instance (for claudemux)
                  - ws_manager: WebSocketManager instance (for opencode)

    Returns:
        Backend instance

    Raises:
        ValueError: If backend name is unknown
    """
    if name == "claudemux":
        from repowire.backends.claudemux import ClaudemuxBackend

        # Extract query_tracker if provided
        query_tracker = kwargs.pop("query_tracker", None)
        return ClaudemuxBackend(query_tracker=query_tracker)
    elif name == "opencode":
        from repowire.backends.opencode import OpencodeBackend

        # Remove query_tracker if present (opencode doesn't use it directly)
        kwargs.pop("query_tracker", None)
        return OpencodeBackend(**kwargs)
    else:
        raise ValueError(f"Unknown backend: {name}")


__all__ = ["get_backend"]
