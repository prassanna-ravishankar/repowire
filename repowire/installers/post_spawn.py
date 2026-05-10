"""Per-backend post-spawn lifecycle middleware.

Each backend gets a chance to nudge its freshly spawned tmux pane through its
own startup quirks (codex's lazy SessionStart, future runtimes' init dialogs,
etc.) so the rest of the system can rely on the normal hook lifecycle.

Backends that fire SessionStart at process boot (claude-code, opencode) are
no-ops. Codex requires a tmux send-keys nudge because its SessionStart hook
fires on first user prompt, not at boot — without this nudge the peer would
sit invisible-from-outside until a human types something.

Default the message to a short branded warmup; callers (the MCP spawn_peer
tool) can override with a per-spawn intent string.
"""

from __future__ import annotations

import asyncio
import logging
import shutil

from repowire.config.models import AgentType

logger = logging.getLogger(__name__)

DEFAULT_WARMUP_TEMPLATE = (
    "Hi from repowire. You have been spawned in {path} under circle "
    "{circle}. Standing by for instructions."
)


async def post_spawn_warmup(
    backend: AgentType,
    pane_id: str,
    *,
    path: str,
    circle: str,
    message: str | None = None,
) -> None:
    """Run the post-spawn lifecycle nudge for a backend.

    Args:
        backend: Which agent runtime is hosting the spawned pane.
        pane_id: tmux pane id (e.g. "%42") to drive.
        path: Project path the agent is starting in. Used in the default message.
        circle: Circle the peer was spawned into. Used in the default message.
        message: Optional override for the default warmup text.

    Best-effort. Logs and swallows any failure -- a stalled warmup must not
    block the spawn flow or any other peer's work.
    """
    try:
        if backend == AgentType.CODEX:
            text = message or DEFAULT_WARMUP_TEMPLATE.format(path=path, circle=circle)
            await _codex_warmup(pane_id, text)
        else:
            # claude-code, opencode, gemini: SessionStart (or equivalent) fires
            # at boot, so no nudge needed. If a future backend needs one, add a
            # branch here.
            return
    except Exception as e:
        logger.warning("post_spawn_warmup failed for %s pane %s: %s", backend, pane_id, e)


async def _codex_warmup(pane_id: str, message: str) -> None:
    """Drive codex's startup lifecycle by sending a warmup prompt.

    Codex's SessionStart hook fires lazily on first user prompt submission,
    not at process boot. Without a nudge, peers spawned via /spawn would not
    register their websocket transport with the daemon until a human typed
    something. Sending a synthetic first prompt fires the hook within seconds.

    Sequence (rule-based, validated by probe at 20/20 success across detached
    and attached panes on codex 0.130.0):
      1. Sleep 8s   -- codex boot + first-run update budget
      2. C-m x 4    -- dismiss any startup dialog (trust prompt, update prompt)
                       300ms apart; empty submits are no-ops in codex's TUI
      3. Sleep 1s   -- let the TUI settle into idle state
      4. send-keys message + C-m   -- the actual warmup, fires SessionStart

    Total runtime: ~10-13s. Runs as a background task so the /spawn POST
    response is not blocked.
    """
    if not pane_id:
        return
    if not shutil.which("tmux"):
        logger.warning("codex warmup: tmux binary not found, skipping")
        return

    await asyncio.sleep(8)

    for _ in range(4):
        await _tmux_send(pane_id, "C-m")
        await asyncio.sleep(0.3)

    await asyncio.sleep(1)

    await _tmux_send(pane_id, message, literal=True)
    await asyncio.sleep(0.2)
    await _tmux_send(pane_id, "C-m")


async def _tmux_send(pane_id: str, text: str, *, literal: bool = False) -> None:
    """Send a key/literal to a tmux pane via the tmux binary.

    Args:
        pane_id: Target tmux pane (e.g. "%42").
        text: Literal text to type (with `literal=True`) or a tmux key name
              like "C-m" or "Enter".
        literal: When True, pass `-l` so the bytes are not parsed as key names.
    """
    args = ["tmux", "send-keys", "-t", pane_id]
    if literal:
        args.append("-l")
    args.append(text)
    proc = await asyncio.create_subprocess_exec(
        *args,
        stdout=asyncio.subprocess.DEVNULL,
        stderr=asyncio.subprocess.PIPE,
    )
    _, err = await proc.communicate()
    if proc.returncode != 0:
        msg = err.decode("utf-8", "replace").strip()
        logger.warning("tmux send-keys failed (rc=%d): %s", proc.returncode, msg)
