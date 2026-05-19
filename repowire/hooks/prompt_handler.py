#!/usr/bin/env python3
"""Handle UserPromptSubmit / BeforeAgent hook - marks peer as BUSY."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.adapters import hook_output, normalize
from repowire.hooks.utils import get_display_name, update_status


def _maybe_spawn_chat_delta_streamer(
    transcript_path: str | None, pane_id: str | None,
) -> None:
    """Spawn the per-turn transcript tailer if the experiment is on.

    Best-effort: config-load or spawn failures must not break the prompt
    hook. Backend-gated to claude-code — other agents land here through
    the adapter but their transcript shapes aren't supported yet.
    """
    if not transcript_path or not pane_id:
        return
    try:
        from repowire.config.models import load_config

        cfg = load_config()
    except Exception:
        return
    if not cfg.experiments.chat_turn_streaming:
        return
    if not Path(transcript_path).expanduser().exists():
        return

    # Single-owner reset: if a previous streamer for this pane is still alive
    # (e.g. user submitted a second prompt before Stop cleared the first, or
    # Stop's pidfile unlink failed), terminate it before starting the new one.
    # Otherwise the predecessor would keep its old turn_id baseline and
    # incorrectly group the new turn's blocks under it.
    from repowire.hooks.chat_delta_streamer import (
        terminate_live_streamer,
    )

    terminate_live_streamer(pane_id)

    try:
        subprocess.Popen(  # noqa: S603 — fixed argv, no shell
            [
                sys.executable,
                "-m",
                "repowire.hooks.chat_delta_streamer",
                "--transcript",
                str(Path(transcript_path).expanduser()),
                "--peer",
                get_display_name(),
                "--pane-id",
                pane_id,
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
    except OSError as e:
        print(
            f"repowire prompt: chat_delta streamer spawn failed: {e}",
            file=sys.stderr,
        )


def main(backend: str = "claude-code") -> int:
    """Main entry point for prompt hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire prompt: invalid JSON input: {e}", file=sys.stderr)
        return 0

    payload = normalize(input_data, backend)

    if payload.event != "UserPromptSubmit":
        return 0

    pane_id = get_pane_id()
    if pane_id:
        if not update_status(pane_id, "busy", use_pane_id=True, turn_state="working"):
            print(
                f"repowire prompt: failed to update status for pane {pane_id}",
                file=sys.stderr,
            )

    if backend == "claude-code":
        _maybe_spawn_chat_delta_streamer(payload.transcript_path, pane_id)

    hook_output(backend)
    return 0


if __name__ == "__main__":
    sys.exit(main())
