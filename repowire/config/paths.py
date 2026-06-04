"""Filesystem paths used by Repowire configuration and runtime caches."""

from __future__ import annotations

from pathlib import Path

CACHE_DIR: Path = Path.home() / ".cache" / "repowire"
"""Runtime cache directory for logs and transient state."""


def get_config_dir() -> Path:
    """Get the Repowire config directory."""
    return Path.home() / ".repowire"


def get_config_path() -> Path:
    """Get the Repowire config file path."""
    return get_config_dir() / "config.yaml"
