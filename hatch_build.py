"""Build hook: include web/out only when it exists.

Prebuilt UI assets (web/out) ship in published wheels/sdists, but a clean
checkout has no web/out until `npm run build` runs. A static force-include
breaks editable installs and `uv sync` from a fresh clone. This hook makes
the inclusion conditional so backend installs work without the UI build.
"""

from __future__ import annotations

from pathlib import Path

from hatchling.builders.hooks.plugin.interface import BuildHookInterface


class WebOutHook(BuildHookInterface):
    PLUGIN_NAME = "web-out"

    def initialize(self, version: str, build_data: dict) -> None:
        web_out = Path(self.root) / "web" / "out"
        if web_out.is_dir() and any(web_out.iterdir()):
            build_data.setdefault("force_include", {})[str(web_out)] = "web/out"
