"""Hardened tmux paste injection, shared across transports.

A single tmux-paste implementation used by both the ws-hook (client-side,
normal message delivery) and the daemon's post-spawn seed (server-side wake-up
for backends whose hook registers lazily). Two near-duplicate implementations
existed before — the hook's was hardened against the runtime's bracketed-paste
heuristic swallowing the submit Enter, the spawn-seed one was not — and a seed
message into a freshly booted pane was injected through the un-hardened path.

The pattern (Gastown's battle-tested NudgeSession):
1. If pane is in copy-mode, cancel out first (otherwise ``-l`` lands in the
   copy buffer and Enter triggers a selection action instead of submit).
2. Send text in literal mode (bracketed paste).
3. Debounce, scaled with text length — the runtime's paste heuristic must
   settle before Enter or it swallows it as a newline.
4. Explicitly close bracketed paste mode with ESC[201~.
5. Enter — submits.
6. Verify: if the composer still holds the text, the paste heuristic ate the
   Enter — nudge once more (an extra Enter on an empty composer is a no-op,
   but we only resend on positive evidence).
"""

from __future__ import annotations

import logging
import subprocess
import time

logger = logging.getLogger(__name__)

# tmux composer prompt glyphs across the supported runtimes. The bottom-most
# line starting with one of these is the live input line.
_COMPOSER_PROMPT_PREFIXES = ("❯", "›", "> ")


def _pane_in_copy_mode(pane_id: str) -> bool:
    """True if the pane is in tmux copy-mode. False on any query failure."""
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{pane_in_mode}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False
    if result.returncode != 0:
        return False
    return result.stdout.strip() == "1"


def _capture_pane(pane_id: str) -> str | None:
    """Capture visible pane text, or None on any failure."""
    try:
        result = subprocess.run(
            ["tmux", "capture-pane", "-t", pane_id, "-p"],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    return result.stdout


def _composer_prompt_present(pane_text: str) -> bool:
    """True if the captured pane shows a composer prompt line."""
    return any(
        line.lstrip().startswith(_COMPOSER_PROMPT_PREFIXES)
        for line in pane_text.splitlines()
    )


def composer_still_holds(pane_id: str, text: str) -> bool:
    """Heuristic: is the injected text still sitting unsubmitted in the
    composer?

    A submitted prompt also remains visible in the transcript, so presence of
    the text alone proves nothing. The distinguishing feature is the
    bottom-most composer prompt line: after a successful submit it is empty
    (a bare prompt), while a swallowed Enter leaves our text in it. False on
    any capture failure — callers must not retry on uncertainty.
    """
    pane_text = _capture_pane(pane_id)
    if pane_text is None:
        return False
    prefix = text.splitlines()[0][:40].strip() if text.strip() else ""
    if not prefix:
        return False
    prompt_lines = [
        line for line in pane_text.splitlines()
        if line.lstrip().startswith(_COMPOSER_PROMPT_PREFIXES)
    ]
    if not prompt_lines:
        return False
    return prefix in prompt_lines[-1]


def wait_for_composer_ready(
    pane_id: str,
    *,
    timeout: float,
    poll: float = 0.5,
    stable_polls: int = 3,
) -> bool:
    """Poll capture-pane until the pane looks ready, or timeout elapses.

    Two ready signals, in priority order:
      * **glyph** — a known composer prompt line (``❯``/``›``/``> ``) is the
        fast path for the supported runtimes.
      * **stable** — for a backend with an unrecognized prompt glyph, falling
        through to the full timeout would regress latency vs the old fixed
        sleep. So if the capture is non-empty and unchanged across
        ``stable_polls`` consecutive polls, treat it as ready. This bounds
        unknown-glyph backends to roughly the old latency.

    A false positive here is acceptable: the prior behavior injected blindly
    after a fixed sleep, so a too-early "ready" is never worse than the
    baseline — and the hardened ``inject_text`` paste path tolerates it.

    Returns True once ready, False if the budget is exhausted without either
    signal (callers should still attempt their paste as a fallback, but log
    that readiness was not confirmed). Capture failure is treated as "not yet
    ready" and resets the stability streak. Logs which signal fired.
    """
    deadline = time.monotonic() + timeout
    last_text: str | None = None
    stable_count = 0
    while True:
        pane_text = _capture_pane(pane_id)
        if pane_text is not None and _composer_prompt_present(pane_text):
            logger.debug("Pane %s ready (glyph)", pane_id)
            return True
        if pane_text is not None and pane_text.strip():
            if pane_text == last_text:
                stable_count += 1
                if stable_count >= stable_polls:
                    logger.debug(
                        "Pane %s ready (stable content, no glyph match)", pane_id
                    )
                    return True
            else:
                stable_count = 1
                last_text = pane_text
        else:
            stable_count = 0
            last_text = None
        if time.monotonic() >= deadline:
            logger.debug("Pane %s readiness not confirmed (timeout)", pane_id)
            return False
        time.sleep(poll)


def inject_text(pane_id: str, text: str) -> bool:
    """Paste ``text`` into a tmux pane and submit it (hardened).

    Synchronous and subprocess-driven (sleeps), so daemon callers must wrap it
    in ``asyncio.to_thread``. Returns True if the send sequence completed,
    False if a tmux command failed.
    """
    try:
        if _pane_in_copy_mode(pane_id):
            subprocess.run(
                ["tmux", "send-keys", "-t", pane_id, "-X", "cancel"],
                capture_output=True,
                check=True,
            )
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "-l", text],
            capture_output=True,
            check=True,
        )
        time.sleep(min(0.5 + len(text) / 4000.0, 1.5))
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "-H", "1b", "5b", "32", "30", "31", "7e"],
            capture_output=True,
            check=True,
        )
        time.sleep(0.1)
        subprocess.run(
            ["tmux", "send-keys", "-t", pane_id, "Enter"],
            capture_output=True,
            check=True,
        )
        time.sleep(0.4)
        if composer_still_holds(pane_id, text):
            logger.warning(
                "Pane %s composer still holds injected text after Enter; nudging once",
                pane_id,
            )
            subprocess.run(
                ["tmux", "send-keys", "-t", pane_id, "Enter"],
                capture_output=True,
                check=True,
            )
        return True
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        logger.error(f"Failed to send keys to {pane_id}: {e}")
        return False
