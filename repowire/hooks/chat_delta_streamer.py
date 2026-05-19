"""Block-level chat_turn_delta streamer.

Spawned by the prompt hook at turn start (when ``experiments.chat_turn_streaming``
is on). Tails the Claude Code transcript JSONL appended during the active turn
and posts a ``/events/chat_delta`` for every new assistant text / tool_use
block. Exits when the Stop hook clears the pidfile, when the transcript stops
growing past an idle window, or when a hard wall-clock cap is hit.

Why polling here is acceptable: this is a transient per-turn helper, not a
daemon-side loop. The lazy-repair doctrine targets the daemon and long-lived
processes; a short-lived 200ms tail bound to one turn is the simplest viable
shape until ACP ``chat/update`` gives us a push channel.

Block-level, not token-level: Claude Code writes one *completed* content
block per JSONL line. claudecodeui gets token frames because it spawns
``claude`` as a child and reads stdout JSON — a path the hook model never
sees. Block-level still delivers the §4.3 UX win (long turns render
progressively instead of one big landing).
"""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

from repowire.hooks.utils import daemon_post, pane_logs_dir
from repowire.session.transcript import _summarize_tool_input

POLL_INTERVAL_S = 0.2
"""How often to re-stat the transcript. 200ms keeps perceived latency low
without burning CPU on a quiet file."""

IDLE_EXIT_AFTER_S = 90.0
"""Exit after this many seconds with no new bytes. Stop hook normally clears
the pidfile first; this is a fallback for missed Stops."""

WALL_CLOCK_CAP_S = 1800.0
"""Hard cap (30 min). A streamer this old means something went wrong."""


def _sanitize_pane(pane_id: str) -> str:
    return pane_id.replace("%", "").replace("/", "").replace("\\", "") or "unknown"


def streamer_pid_path(pane_id: str) -> Path:
    """Pidfile for the streamer attached to a pane. Stop handler reads/deletes."""
    return pane_logs_dir() / f"chat-delta-streamer-{_sanitize_pane(pane_id)}.pid"


def streamer_lock_path(pane_id: str) -> Path:
    """Lockfile guarding the single-owner streamer for a pane.

    Held under flock for the streamer's whole lifetime. A second streamer
    spawning for the same pane fails the non-blocking lock and exits cleanly
    rather than racing on the transcript.
    """
    return pane_logs_dir() / f"chat-delta-streamer-{_sanitize_pane(pane_id)}.lock"


def _pid_alive(pid: int) -> bool:
    """True if `pid` exists. EPERM counts as alive (foreign owner)."""
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def terminate_live_streamer(pane_id: str, *, wait_s: float = 0.5) -> bool:
    """If a streamer for `pane_id` is alive, signal it to exit and wait briefly.

    Used by the prompt hook to evict a predecessor before spawning a fresh
    streamer for the next turn. Returns True if we tried to terminate one,
    False if no live owner was found. Best-effort — never raises.

    Strategy: remove the pidfile (the streamer polls it every POLL_INTERVAL_S
    and exits on absence) AND send SIGTERM as a belt-and-suspenders so the
    predecessor stops even if it's mid-`daemon_post`. We deliberately use the
    same pidfile-removal signal Stop uses; no new wire.

    Trust model: the pidfile is treated as authoritative for "what pid is the
    streamer owner". The SIGTERM is sent without process-identity verification
    (no `comm` / cmdline check, no flock probe before kill). PID reuse on a
    long-lived host could in principle cause us to SIGTERM an unrelated
    process that landed on the same numeric pid after a crashed streamer.
    Acceptable for this experimental hook: the cache dir is per-user, the
    pidfile lifetime is bounded by Stop / idle-cap (~90s), and SIGTERM is
    delivered to the user's own uid only (kernel returns EPERM otherwise).
    Tighten via the lockfile metadata if the experiment graduates.
    """
    pid_path = streamer_pid_path(pane_id)
    try:
        pid_text = pid_path.read_text().strip()
    except OSError:
        return False
    try:
        pid = int(pid_text)
    except ValueError:
        return False
    if not _pid_alive(pid):
        # Stale pidfile from a crashed predecessor — clear it so it doesn't
        # mislead the next liveness probe.
        try:
            pid_path.unlink()
        except OSError:
            pass
        return False

    try:
        pid_path.unlink()
    except OSError:
        pass
    try:
        os.kill(pid, 15)  # SIGTERM
    except (ProcessLookupError, PermissionError):
        return True

    deadline = time.monotonic() + wait_s
    while time.monotonic() < deadline:
        if not _pid_alive(pid):
            return True
        time.sleep(0.02)
    return True


def has_live_streamer(pane_id: str) -> bool:
    """True if a streamer process for `pane_id` is currently alive.

    Checked by the prompt hook before spawning to avoid the duplicate-streamer
    race when two prompt hooks fire back-to-back. We deliberately do *not*
    rely solely on the lockfile: a still-flushing predecessor that already
    released the lock but hasn't exited would slip through. Reading the pid
    and probing it is cheap and conclusive.
    """
    pid_path = streamer_pid_path(pane_id)
    try:
        pid_text = pid_path.read_text().strip()
    except OSError:
        return False
    try:
        pid = int(pid_text)
    except ValueError:
        return False
    return _pid_alive(pid)


def _content_blocks(message: dict[str, Any]) -> list[dict[str, Any]]:
    content = message.get("content")
    if isinstance(content, list):
        return [c for c in content if isinstance(c, dict)]
    return []


def _emit_text(
    peer: str,
    pane_id: str,
    session_id: str | None,
    turn_id: str,
    chunk_index: int,
    text: str,
) -> bool:
    payload = {
        "peer": peer,
        "role": "assistant",
        "turn_id": turn_id,
        "chunk_index": chunk_index,
        "kind": "text",
        "text": text,
        "is_final": False,
        "pane_id": pane_id,
    }
    if session_id:
        payload["session_id"] = session_id
    return daemon_post("/events/chat_delta", payload) is not None


def _emit_tool_use(
    peer: str,
    pane_id: str,
    session_id: str | None,
    turn_id: str,
    chunk_index: int,
    name: str,
    tool_input: Any,
) -> bool:
    summary = _summarize_tool_input(name, tool_input)
    payload = {
        "peer": peer,
        "role": "assistant",
        "turn_id": turn_id,
        "chunk_index": chunk_index,
        "kind": "tool_use",
        "text": f"{name}: {summary}" if summary else name,
        "tool_call": {"name": name, "input": summary},
        "is_final": False,
        "pane_id": pane_id,
    }
    if session_id:
        payload["session_id"] = session_id
    return daemon_post("/events/chat_delta", payload) is not None


def _tail_transcript(
    transcript_path: Path, peer: str, pane_id: str, session_id: str | None, pid_path: Path,
) -> None:
    """Inner tail loop. Exits when pidfile is removed, idle, or wall-clock cap.

    Separated from `run()` so the lock-ownership guard wraps the whole
    lifetime, including transcript-open and final cleanup.
    """
    started = time.monotonic()
    last_growth = started
    # Skip lines that already existed at startup so a fresh prompt doesn't
    # replay the entire prior transcript as deltas.
    last_size = transcript_path.stat().st_size if transcript_path.exists() else 0
    line_buffer = b""

    # turn_id is the uuid of the first assistant message we see after start.
    # Falls back to "<pane>-<started_ts>" if the turn happens to be pure tool_use.
    turn_id: str | None = None
    chunk_index = 0

    try:
        with open(transcript_path, "rb") as fh:
            fh.seek(last_size)
            while True:
                # Pidfile-driven exit: Stop hook deletes it to signal us out.
                if not pid_path.exists():
                    break
                # Foreign owner check: a fresh prompt may have spawned a new
                # streamer that rewrote the pidfile before our finally clause.
                # If the pid no longer matches ours, exit immediately without
                # touching the file in cleanup.
                try:
                    if pid_path.read_text().strip() != str(os.getpid()):
                        return
                except OSError:
                    break

                now = time.monotonic()
                if now - started > WALL_CLOCK_CAP_S:
                    break
                if now - last_growth > IDLE_EXIT_AFTER_S:
                    break

                chunk = fh.read()
                if not chunk:
                    time.sleep(POLL_INTERVAL_S)
                    continue

                last_growth = now
                line_buffer += chunk
                while b"\n" in line_buffer:
                    raw_line, line_buffer = line_buffer.split(b"\n", 1)
                    raw_line = raw_line.strip()
                    if not raw_line:
                        continue
                    try:
                        entry = json.loads(raw_line)
                    except json.JSONDecodeError:
                        continue
                    if entry.get("type") != "assistant":
                        continue
                    message = entry.get("message", {})
                    if not isinstance(message, dict):
                        continue
                    if turn_id is None:
                        turn_id = entry.get("uuid") or f"{pane_id}-{int(started)}"
                    for block in _content_blocks(message):
                        block_type = block.get("type")
                        if block_type == "text":
                            text = block.get("text") or ""
                            if not text.strip():
                                continue
                            if _emit_text(peer, pane_id, session_id, turn_id, chunk_index, text):
                                chunk_index += 1
                        elif block_type == "tool_use":
                            name = block.get("name", "unknown")
                            tool_input = block.get("input", {})
                            if _emit_tool_use(
                                peer, pane_id, session_id, turn_id, chunk_index, name, tool_input,
                            ):
                                chunk_index += 1
    except FileNotFoundError:
        # Transcript not yet created when we started, or rotated. Either way
        # the Stop hook will deliver the full turn — nothing useful left to do.
        pass


def run(
    transcript_path: Path,
    peer: str,
    pane_id: str,
    session_id: str | None = None,
) -> int:
    """Single-owner streamer entry point.

    Acquires a non-blocking flock on the per-pane lockfile for the whole
    lifetime. If a prior streamer still holds it (e.g. two prompt hooks
    fired before Stop cleared the old one), exit cleanly without touching
    pidfile or transcript — the live owner stays in charge of its turn.
    The pidfile is rewritten *under the lock* so liveness probes from the
    prompt hook are always consistent with the active owner.
    """
    lock_path = streamer_lock_path(pane_id)
    pid_path = streamer_pid_path(pane_id)
    try:
        lock_fd = open(lock_path, "w")  # noqa: SIM115
    except OSError as e:
        print(f"chat-delta-streamer: lockfile open failed: {e}", file=sys.stderr)
        return 1

    try:
        try:
            fcntl.flock(lock_fd.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            # Another streamer owns this pane. Don't touch pidfile or transcript.
            return 0

        try:
            pid_path.write_text(str(os.getpid()))
        except OSError as e:
            print(f"chat-delta-streamer: pidfile write failed: {e}", file=sys.stderr)
            return 1

        try:
            _tail_transcript(transcript_path, peer, pane_id, session_id, pid_path)
        finally:
            # Owner-checked unlink: only remove the pidfile if it still names
            # us. A fresh streamer that overwrote the pid will release the
            # lock on its own exit and clean up its own pid.
            try:
                current = pid_path.read_text().strip() if pid_path.exists() else None
                if current == str(os.getpid()):
                    pid_path.unlink()
            except OSError:
                pass
    finally:
        try:
            fcntl.flock(lock_fd.fileno(), fcntl.LOCK_UN)
        except OSError:
            pass
        lock_fd.close()

    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="repowire chat_turn_delta streamer")
    parser.add_argument("--transcript", required=True, help="Path to transcript JSONL")
    parser.add_argument("--peer", required=True, help="Peer display name")
    parser.add_argument("--pane-id", required=True, help="Tmux pane id (state key)")
    parser.add_argument("--session-id", help="Hook/runtime session id for dashboard scoping")
    args = parser.parse_args(argv)
    return run(Path(args.transcript).expanduser(), args.peer, args.pane_id, args.session_id)


if __name__ == "__main__":
    sys.exit(main())
