"""Per-capability probes for `codex-acp` (Zed adapter for Codex).

Highest-risk rows: C4 (tool_call lifecycle) + C5 (request_permission) — Codex's
tool-call surface differs from ACP's, so the translation layer is the gating
risk for ACP-first migration.
"""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from probes._common import emit_results, run_all  # noqa: E402

ADAPTER = "codex-acp"
COMMAND = ("codex-acp",)


if __name__ == "__main__":
    emit_results(ADAPTER, asyncio.run(run_all(ADAPTER, COMMAND)))
