"""Per-capability probes for `pi-acp` (less mature; weight C1-C3 as gating)."""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from probes._common import emit_results, run_all  # noqa: E402

ADAPTER = "pi-acp"
COMMAND = ("pi-acp",)


if __name__ == "__main__":
    emit_results(ADAPTER, asyncio.run(run_all(ADAPTER, COMMAND)))
