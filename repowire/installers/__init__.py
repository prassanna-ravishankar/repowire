"""Agent-type-specific installers."""

from repowire.installers.claude_code import (
    check_hooks_installed,
    install_hooks,
    uninstall_hooks,
)
from repowire.installers.opencode import (
    check_plugin_installed,
    install_plugin,
    uninstall_plugin,
)

__all__ = [
    "install_hooks",
    "uninstall_hooks",
    "check_hooks_installed",
    "install_plugin",
    "uninstall_plugin",
    "check_plugin_installed",
]
