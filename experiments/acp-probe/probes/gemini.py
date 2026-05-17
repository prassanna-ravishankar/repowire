"""Per-capability probes for `gemini --experimental-acp` (native adapter).

Native flag — expected to pass C1-C10. C11/C12 unknown going in.

Note on C3: streaming chunk count is non-deterministic. Gemini sometimes
coalesces a short response into a single frame even when prompted for
multi-line output. A single "partial" observation is evidence that streaming
is best-effort, not guaranteed.
"""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from probes._common import emit_results, run_all  # noqa: E402

ADAPTER = "gemini"
COMMAND = ("gemini", "--experimental-acp")


if __name__ == "__main__":
    emit_results(ADAPTER, asyncio.run(run_all(ADAPTER, COMMAND)))
