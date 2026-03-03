"""Service installation for repowire daemon."""

from repowire.service.installer import (
    get_platform,
    get_service_status,
    install_service,
    restart_service,
    uninstall_service,
)

__all__ = [
    "get_platform",
    "install_service",
    "restart_service",
    "uninstall_service",
    "get_service_status",
]
