"""Per-capability probes for `claude-agent-acp` (Zed adapter for Claude Code).

Highest-risk row: C7 (session/load fidelity — Claude CLI's JSONL format may not
round-trip perfectly through the adapter).
"""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from probes._common import emit_results, run_all  # noqa: E402

ADAPTER = "claude-agent-acp"
COMMAND = ("claude-agent-acp",)


if __name__ == "__main__":
    emit_results(ADAPTER, asyncio.run(run_all(ADAPTER, COMMAND)))
