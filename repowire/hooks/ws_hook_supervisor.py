"""Spawn and respawn the per-pane ws-hook process.

The ws-hook is launched once at SessionStart and pinned to its pane via an
exclusive flock plus a pid file. Its internal reconnect loop survives daemon
restarts and transient network failures, but if the *process itself* dies
(OOM, SIGKILL, unhandled exception in a handler) nothing brings it back
until the next SessionStart -- the agent keeps running while inbound asks
silently drop.

`maybe_respawn` is the lazy-repair path: it piggy-backs on Stop hook traffic
to detect a dead pid file, claim the flock, and relaunch the ws-hook using
the persisted pane metadata. No new polling.
"""

from __future__ import annotations

import fcntl
import logging
import os
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

from repowire.hooks.utils import (
    get_pane_file,
    pane_logs_dir,
    read_pane_runtime_metadata,
    write_pane_runtime_metadata,
    ws_hook_lock_path,
    ws_hook_meta_path,
    ws_hook_pid_path,
)

logger = logging.getLogger(__name__)


def link_spawn_ws_hook(
    pane_id: str,
    *,
    peer_id: str,
    display_name: str,
    backend: str,
    cwd: str,
) -> bool:
    """Start a ws-hook for a freshly-adopted (orphan) pane. Returns spawn success.

    First-adoption spawn for ``link`` — distinct from :func:`maybe_respawn`,
    which is a lazy-repair helper that refuses to act without a prior pidfile +
    pane metadata. Here there is no prior hook: claim the pane flock, write the
    pane runtime metadata SessionStart would have written, then spawn the hook.

    Returns False if the lock is contested (another hook is starting/alive for
    this pane) or the hook script is missing — the caller then fails the link
    rather than leaving a half-adopted peer.
    """
    lock_path = ws_hook_lock_path(pane_id)
    try:
        lock_fd = open(lock_path, "w")  # noqa: SIM115
    except OSError as e:
        logger.warning("link_spawn_ws_hook: pane %s lock open failed: %s", pane_id, e)
        return False
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            lock_fd.close()
            logger.warning("link_spawn_ws_hook: pane %s lock contested", pane_id)
            return False
        # Any failure past this point (metadata write or spawn) returns False so
        # the caller's rollback (clear_pane_runtime_state + unregister) runs —
        # an exception must not escape and leave a registered ghost behind.
        try:
            write_pane_runtime_metadata(
                pane_id,
                {
                    "backend": backend,
                    "cwd": cwd,
                    "display_name": display_name,
                    "peer_id": peer_id,
                },
            )
            new_pid = spawn_ws_hook(
                pane_id=pane_id,
                peer_id=peer_id,
                display_name=display_name,
                backend=backend,
                cwd=cwd,
                lock_fd=lock_fd,
                agent_pid=None,
            )
        except Exception as e:  # noqa: BLE001 — fail closed so the route rolls back
            logger.warning("link_spawn_ws_hook: pane %s spawn failed: %s", pane_id, e)
            return False
        return new_pid is not None
    finally:
        # Child inherited the flock via pass_fds; release our copy.
        lock_fd.close()


def spawn_ws_hook(
    *,
    pane_id: str | None,
    peer_id: str | None,
    display_name: str,
    backend: str,
    cwd: str,
    lock_fd,
    agent_pid: int | None = None,
) -> int | None:
    """Launch websocket_hook.py in the background and write its pid file.

    `lock_fd` must already hold an exclusive flock on `ws_hook_lock_path(pane_id)`.
    The child inherits the flock via `pass_fds`; the caller is responsible for
    closing its own copy of the fd after this returns.

    Returns the child pid, or None if the hook script is missing.
    """
    hook_script = Path(__file__).parent / "websocket_hook.py"
    if not hook_script.exists():
        return None

    pane_file = get_pane_file(pane_id)
    log_dir = pane_logs_dir()
    log_file = open(log_dir / f"ws-hook-{pane_file}.log", "a")  # noqa: SIM115
    try:
        env = os.environ.copy()
        env["REPOWIRE_DISPLAY_NAME"] = display_name
        if peer_id:
            env["REPOWIRE_PEER_ID"] = peer_id
        if agent_pid is not None and agent_pid > 0:
            env["REPOWIRE_AGENT_PID"] = str(agent_pid)
        env["REPOWIRE_BACKEND"] = backend
        # ws-hook reads TMUX_PANE for its pane id. Stop hook respawn runs in
        # a different subprocess context than SessionStart, so set explicitly.
        if pane_id:
            env["TMUX_PANE"] = pane_id
        proc = subprocess.Popen(
            [sys.executable, str(hook_script)],
            stdout=log_file,
            stderr=log_file,
            start_new_session=True,
            cwd=cwd,
            env=env,
            pass_fds=(lock_fd.fileno(),),
        )
        # Atomic write: concurrent writers (Stop-hook respawn vs SessionStart)
        # racing plain write_text have produced interleaved garbage pids.
        pid_path = ws_hook_pid_path(pane_id)
        tmp_path = pid_path.with_suffix(".pid.tmp")
        tmp_path.write_text(str(proc.pid))
        os.replace(tmp_path, pid_path)
        return proc.pid
    finally:
        log_file.close()


def _pid_alive(pid: int) -> bool:
    """True if `pid` exists. EPERM also counts as alive (foreign owner)."""
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def maybe_respawn(
    pane_id: str | None,
    *,
    backend: str | None = None,
    cwd: str | None = None,
) -> bool:
    """Restart the ws-hook for `pane_id` iff its prior process is dead.

    Lazy-repair path triggered from the Stop hook. Returns True when a fresh
    ws-hook was launched, False otherwise (no pane, pid still alive, lock
    contested, missing metadata, or hook script absent).

    Best-effort: any failure is swallowed so the Stop hook never breaks an
    agent turn over a respawn miss.
    """
    if not pane_id:
        return False

    try:
        pid_path = ws_hook_pid_path(pane_id)
        try:
            pid_text = pid_path.read_text().strip()
        except OSError:
            # No pid file: either ws-hook was never started for this pane, or
            # it shut down cleanly (e.g. PaneUnsafeError clears state). Either
            # way, respawning here would race with SessionStart. Skip.
            return False

        try:
            old_pid = int(pid_text)
        except ValueError:
            return False

        if _pid_alive(old_pid):
            return False

        # Claim the pane's lock non-blocking. If contested, some other
        # ws-hook is in the middle of starting or already alive -- leave it.
        lock_path = ws_hook_lock_path(pane_id)
        lock_fd = open(lock_path, "w")  # noqa: SIM115
        try:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                lock_fd.close()
                return False

            metadata = read_pane_runtime_metadata(pane_id)
            metadata_cwd = metadata.get("cwd")
            display_name = metadata.get("display_name")
            metadata_backend = metadata.get("backend") or "claude-code"
            peer_id = metadata.get("peer_id")
            metadata_agent_pid = metadata.get("agent_pid")
            if isinstance(metadata_agent_pid, int):
                agent_pid = metadata_agent_pid
            elif isinstance(metadata_agent_pid, str) and metadata_agent_pid.isdigit():
                agent_pid = int(metadata_agent_pid)
            else:
                agent_pid = None
            if not metadata_cwd or not display_name:
                # Without these we can't recreate the prior connect state.
                # Drop the stale pid file so a future SessionStart isn't
                # confused, but don't spawn blindly.
                try:
                    pid_path.unlink()
                except OSError:
                    pass
                return False
            if not backend or not cwd:
                logger.warning(
                    "ws-hook respawn rejected: metadata claims backend=%s cwd=%s "
                    "but current hook reports backend=%s cwd=%s (pane %s)",
                    metadata_backend,
                    metadata_cwd,
                    backend,
                    cwd,
                    pane_id,
                )
                return False
            if metadata_backend != backend or metadata_cwd != cwd:
                logger.warning(
                    "ws-hook respawn rejected: metadata claims backend=%s cwd=%s "
                    "but current hook reports backend=%s cwd=%s (pane %s)",
                    metadata_backend,
                    metadata_cwd,
                    backend,
                    cwd,
                    pane_id,
                )
                return False

            new_pid = spawn_ws_hook(
                pane_id=pane_id,
                peer_id=peer_id,
                display_name=display_name,
                backend=metadata_backend,
                cwd=metadata_cwd,
                lock_fd=lock_fd,
                agent_pid=agent_pid,
            )
            return new_pid is not None
        finally:
            # Child inherited the flock via pass_fds; releasing our copy is
            # safe and required so we don't leak the descriptor.
            lock_fd.close()
    except Exception as e:
        print(f"repowire: ws-hook respawn failed: {e}", file=sys.stderr)
        return False


def startup_respawn_ws_hook(
    pane_id: str,
    *,
    peer_id: str,
    display_name: str,
    backend: str,
    cwd: str,
    agent_pid: int,
) -> bool:
    """Start a ws-hook at daemon boot for a strictly proven peer.

    This helper is intentionally narrower than ``link_spawn_ws_hook`` (manual
    adoption) and ``maybe_respawn`` (Stop-hook lazy repair). The caller must
    already have joined durable peer identity to live pane/runtime evidence.
    """
    lock_path = ws_hook_lock_path(pane_id)
    try:
        lock_fd = open(lock_path, "w")  # noqa: SIM115
    except OSError as e:
        logger.warning("startup_respawn_ws_hook: pane %s lock open failed: %s", pane_id, e)
        return False
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return False

        metadata = read_pane_runtime_metadata(pane_id)
        metadata_peer_id = metadata.get("peer_id")
        cert = metadata.get("birth_certificate")
        cert_peer_id = cert.get("peer_id") if isinstance(cert, dict) else None
        metadata_backend = metadata.get("backend")
        metadata_cwd = metadata.get("cwd")
        normalized_metadata_cwd = (
            os.path.normpath(os.path.abspath(metadata_cwd))
            if isinstance(metadata_cwd, str)
            else None
        )
        normalized_cwd = os.path.normpath(os.path.abspath(cwd))
        peer_matches = metadata_peer_id == peer_id or cert_peer_id == peer_id
        if (
            not peer_matches
            or metadata_backend != backend
            or normalized_metadata_cwd != normalized_cwd
        ):
            logger.warning(
                "startup_respawn_ws_hook rejected: metadata peer/backend/cwd "
                "mismatch for pane %s (peer=%s backend=%s cwd=%s)",
                pane_id,
                peer_id,
                backend,
                cwd,
            )
            return False

        new_pid = spawn_ws_hook(
            pane_id=pane_id,
            peer_id=peer_id,
            display_name=display_name,
            backend=backend,
            cwd=cwd,
            lock_fd=lock_fd,
            agent_pid=agent_pid,
        )
        return new_pid is not None
    except Exception as e:
        logger.warning("startup_respawn_ws_hook failed for pane %s: %s", pane_id, e)
        return False
    finally:
        lock_fd.close()


# ---------------------------------------------------------------------------
# Orphan sweep
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class OrphanReport:
    """One ws-hook process the sweep judged orphaned (or would, on dry_run)."""

    pid: int
    pane_id: str | None
    peer_id: str | None
    reason: str
    killed: bool


def _list_ws_hook_pids() -> dict[int, str] | None:
    """Live websocket_hook processes as {pid: command}; None if ps failed."""
    try:
        result = subprocess.run(
            ["ps", "-axo", "pid=,command="],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.SubprocessError, OSError):
        return None
    if result.returncode != 0:
        return None
    procs: dict[int, str] = {}
    for line in result.stdout.splitlines():
        pid_str, _, command = line.strip().partition(" ")
        if pid_str.isdigit() and "websocket_hook.py" in command:
            procs[int(pid_str)] = command
    return procs


def _live_tmux_panes() -> dict[str, int] | None:
    """Conclusive {pane_id: pane_pid} for all live panes.

    Returns {} only when the tmux server itself answered "no panes exist",
    and None when the listing was inconclusive — callers must not treat None
    as "every pane is dead".
    """
    try:
        result = subprocess.run(
            ["tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_pid}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except FileNotFoundError:
        # tmux missing from *this* process's PATH (e.g. a launchd service
        # environment) says nothing about the user's tmux server — treating
        # it as "no panes" would mass-kill live hooks.
        return None
    except (subprocess.SubprocessError, OSError):
        return None
    if result.returncode != 0:
        return {} if "no server" in result.stderr.lower() else None
    panes: dict[str, int] = {}
    for line in result.stdout.splitlines():
        pane_id, _, pid_str = line.strip().partition("\t")
        if pane_id and pid_str.isdigit():
            panes[pane_id] = int(pid_str)
    return panes


def _terminate(pid: int) -> bool:
    """SIGTERM with a 2s grace period, then SIGKILL. True if the pid is gone.

    Revalidates the pid still belongs to a ws-hook right before signaling, so
    pid reuse inside the sweep window cannot kill an unrelated process.
    """
    try:
        recheck = subprocess.run(
            ["ps", "-o", "command=", "-p", str(pid)],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.SubprocessError, OSError):
        return False
    if recheck.returncode != 0:
        return True  # already gone
    if "websocket_hook.py" not in recheck.stdout:
        return False  # pid reused by something else — do not touch it
    try:
        os.kill(pid, signal.SIGTERM)
    except OSError:
        return True
    for _ in range(20):
        if not _pid_alive(pid):
            return True
        time.sleep(0.1)
    try:
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass
    return not _pid_alive(pid)


def sweep_orphan_ws_hooks(*, dry_run: bool = False) -> list[OrphanReport]:
    """Find (and unless dry_run, kill) ws-hook processes whose agent is gone.

    Conclusive evidence only — a hook is an orphan when:
      - no pid file claims it (a newer hook took over its pane), or
      - tmux authoritatively reports its pane gone, or
      - its pane's process subtree contains only shells (agent quit, shell
        remains — the classic orphan that self-certifies pings as safe).
    Anything inconclusive (ps/tmux unavailable) is left alone, and a dead
    recorded agent_pid is never sufficient by itself (stale pids happen): a
    wrongly killed hook breaks a live peer, a missed orphan dies on the next
    sweep.
    """
    from repowire.hooks.websocket_hook import (
        _build_ps_child_map,
        _capture_baseline_from_subtree,
    )

    procs = _list_ws_hook_pids()
    if not procs:
        return []
    live_panes = _live_tmux_panes()

    owned: dict[int, str] = {}
    for pid_path in pane_logs_dir().glob("ws-hook-*.pid"):
        token = pid_path.stem.removeprefix("ws-hook-")
        try:
            pid = int(pid_path.read_text().strip())
        except (OSError, ValueError):
            continue
        owned[pid] = f"%{token}" if token.isdigit() else token

    ps_result = _build_ps_child_map()

    reports: list[OrphanReport] = []
    for pid in sorted(procs):
        pane_id = owned.get(pid)
        meta = read_pane_runtime_metadata(pane_id) if pane_id else {}

        # NOTE: a dead recorded agent_pid is deliberately NOT a kill criterion
        # on its own — recorded pids can be stale (older hook versions wrote
        # shell or pre-/clear pids), and acting on one killed hooks serving
        # live agents. A genuinely dead agent leaves a shell-only subtree,
        # which the criterion below catches conclusively.
        reason: str | None = None
        if pane_id is None:
            reason = "no pid file claims this ws-hook (superseded for its pane)"
        elif live_panes is not None and pane_id not in live_panes:
            reason = f"pane {pane_id} no longer exists"
        elif (
            live_panes is not None
            and pane_id in live_panes
            and ps_result is not None
            and _capture_baseline_from_subtree(live_panes[pane_id], *ps_result) is None
        ):
            reason = f"pane {pane_id} subtree contains only shells"
        if reason is None:
            continue

        killed = False
        if not dry_run:
            killed = _terminate(pid)
            if killed and pane_id is not None:
                pid_path = ws_hook_pid_path(pane_id)
                try:
                    if pid_path.read_text().strip() == str(pid):
                        pid_path.unlink()
                        ws_hook_meta_path(pane_id).unlink(missing_ok=True)
                except (OSError, ValueError):
                    pass
        peer_id = meta.get("peer_id")
        reports.append(
            OrphanReport(
                pid=pid,
                pane_id=pane_id,
                peer_id=peer_id if isinstance(peer_id, str) else None,
                reason=reason,
                killed=killed,
            )
        )
    return reports
