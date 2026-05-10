"""Shared utilities for hook handlers."""

from __future__ import annotations

import atexit
import fcntl
import json
import os
import sys
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from pathlib import Path

import httpx

from repowire.config.models import DEFAULT_DAEMON_URL

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

# Module-level pooled HTTP client so sequential daemon calls within one hook
# process reuse the same TCP connection via keep-alive. Previously each
# urllib.request.urlopen() call opened a fresh socket, leaving TIME_WAIT
# accumulation heavy enough under multi-session Claude Code workloads to
# exhaust the macOS ephemeral port pool (EADDRNOTAVAIL on outbound IPv4).
_client: httpx.Client | None = None


def _get_client() -> httpx.Client:
    global _client
    if _client is None:
        _client = httpx.Client(base_url=DAEMON_URL, timeout=2.0)
        atexit.register(_client.close)
    return _client


def get_pane_file(pane_id: str | None) -> str:
    """Normalize pane_id for use in cache filenames (strips % and path separators)."""
    sanitized = (pane_id or "unknown").replace("%", "").replace("/", "").replace("\\", "")
    return sanitized or "unknown"


def get_display_name() -> str:
    """Read daemon-assigned display name from REPOWIRE_DISPLAY_NAME env var.

    Set by session_handler after registering with the daemon.
    Falls back to cwd folder name if env var not set.
    """
    name = os.environ.get("REPOWIRE_DISPLAY_NAME")
    if name:
        return name
    return Path.cwd().name


def pending_query_cid_path(pane_id: str | None) -> Path:
    """Path to the pending /query correlation_id file for a pane.

    Single-purpose: only legacy /query cids land here. Ask cids are handled
    transport-side (direct injection of type=ask wire frames) and never use
    a FIFO.
    """
    return pane_logs_dir() / f"pending-query-{get_pane_file(pane_id)}.json"


@contextmanager
def _locked_query_cids(pane_id: str) -> Iterator[list[str]]:
    """Yield the parsed pending-query-cid list under flock; persist on clean exit."""
    path = pending_query_cid_path(pane_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    lock_path = path.with_suffix(path.suffix + ".lock")
    with open(lock_path, "w") as lock_file:
        fcntl.flock(lock_file, fcntl.LOCK_EX)
        try:
            try:
                pending = json.loads(path.read_text()) if path.exists() else []
                if not isinstance(pending, list):
                    pending = []
            except (json.JSONDecodeError, OSError):
                pending = []
            yield pending
            path.write_text(json.dumps(pending))
        finally:
            fcntl.flock(lock_file, fcntl.LOCK_UN)


def push_query_cid(pane_id: str, correlation_id: str) -> None:
    """Append a /query correlation_id to the per-pane FIFO under flock."""
    with _locked_query_cids(pane_id) as pending:
        pending.append(correlation_id)


def pop_query_cid(pane_id: str) -> str | None:
    """Pop the oldest /query correlation_id from the per-pane FIFO under flock."""
    with _locked_query_cids(pane_id) as pending:
        if not pending:
            return None
        return pending.pop(0)


def pane_logs_dir() -> Path:
    """Return the runtime log/state directory for pane-scoped hook files."""
    from repowire.config.models import CACHE_DIR

    path = CACHE_DIR / "logs"
    path.mkdir(parents=True, exist_ok=True)
    return path


def ws_hook_lock_path(pane_id: str | None) -> Path:
    """Lock file guarding the single ws-hook owner for a pane."""
    return pane_logs_dir() / f"ws-hook-{get_pane_file(pane_id)}.lock"


def ws_hook_pid_path(pane_id: str | None) -> Path:
    """PID file for the background ws-hook process."""
    return pane_logs_dir() / f"ws-hook-{get_pane_file(pane_id)}.pid"


def ws_hook_meta_path(pane_id: str | None) -> Path:
    """JSON metadata for the active logical session in a pane."""
    return pane_logs_dir() / f"ws-hook-{get_pane_file(pane_id)}.meta.json"


def ws_hook_legacy_cwd_path(pane_id: str | None) -> Path:
    """Legacy cwd file retained for backward compatibility with older hooks/tests."""
    return pane_logs_dir() / f"ws-hook-{get_pane_file(pane_id)}.cwd"


def read_pane_runtime_metadata(pane_id: str | None) -> dict:
    """Read persisted metadata for the current pane owner."""
    meta_path = ws_hook_meta_path(pane_id)
    try:
        data = json.loads(meta_path.read_text())
        if isinstance(data, dict):
            return data
    except (OSError, json.JSONDecodeError):
        pass

    legacy_cwd = ws_hook_legacy_cwd_path(pane_id)
    try:
        cwd = legacy_cwd.read_text().strip()
    except OSError:
        cwd = ""
    return {"cwd": cwd} if cwd else {}


def write_pane_runtime_metadata(pane_id: str | None, metadata: dict) -> None:
    """Persist metadata for the active logical session in a pane."""
    meta_path = ws_hook_meta_path(pane_id)
    meta_path.write_text(json.dumps(metadata))

    cwd = metadata.get("cwd")
    if cwd:
        ws_hook_legacy_cwd_path(pane_id).write_text(str(cwd))


def reminder_buffer_path(pane_id: str | None) -> Path:
    """Path to the pane's pending reminder text file.

    The Stop hook writes ask-ack reminder text here when this peer has any
    open asks. The next UserPromptSubmit (or Notification/idle_prompt) hook
    reads it, injects, and deletes.
    """
    return pane_logs_dir() / f"reminder-{get_pane_file(pane_id)}.txt"


def consume_reminder_buffer(pane_id: str | None) -> str | None:
    """Read and remove the pending reminder for a pane. Returns None if empty."""
    if not pane_id:
        return None
    path = reminder_buffer_path(pane_id)
    try:
        text = path.read_text().strip()
    except OSError:
        return None
    with suppress(OSError):
        path.unlink()
    return text or None


def write_reminder_buffer(pane_id: str | None, text: str) -> None:
    """Persist reminder text for next-prompt injection. Skips if empty."""
    if not pane_id or not text:
        return
    path = reminder_buffer_path(pane_id)
    try:
        path.write_text(text)
    except OSError as e:
        print(f"repowire: failed to write reminder buffer: {e}", file=sys.stderr)


def clear_pending_cids(pane_id: str | None) -> None:
    """Remove any queued /query correlation IDs for a pane."""
    if not pane_id:
        return

    pending_path = pending_query_cid_path(pane_id)
    lock_path = pending_path.with_suffix(pending_path.suffix + ".lock")
    for path in (pending_path, lock_path):
        with suppress(OSError):
            path.unlink()


def clear_pane_runtime_state(pane_id: str | None) -> None:
    """Clear transient pane-scoped hook state after a pane dies or is taken over."""
    if not pane_id:
        return

    clear_pending_cids(pane_id)
    for path in (
        ws_hook_pid_path(pane_id),
        ws_hook_meta_path(pane_id),
        ws_hook_legacy_cwd_path(pane_id),
        reminder_buffer_path(pane_id),
    ):
        with suppress(OSError):
            path.unlink()


def _log_daemon_error(method: str, path: str, exc: Exception) -> None:
    """Log daemon request failure, including HTTP response body when available."""
    msg = f"repowire: daemon {method} {path} failed: {exc}"
    if isinstance(exc, httpx.HTTPStatusError):
        body = exc.response.text
        if body:
            msg += f" - Body: {body}"
    print(msg, file=sys.stderr)


def daemon_post(path: str, payload: dict, *, timeout: float = 2.0) -> dict | None:
    """POST JSON to daemon. Returns parsed response or None on failure."""
    try:
        resp = _get_client().post(path, json=payload, timeout=timeout)
        resp.raise_for_status()
        return resp.json()
    except (httpx.HTTPError, json.JSONDecodeError) as e:
        _log_daemon_error("POST", path, e)
        return None


def daemon_get(path: str, *, timeout: float = 2.0) -> dict | None:
    """GET from daemon. Returns parsed response or None on failure."""
    try:
        resp = _get_client().get(path, timeout=timeout)
        resp.raise_for_status()
        return resp.json()
    except (httpx.HTTPError, json.JSONDecodeError) as e:
        _log_daemon_error("GET", path, e)
        return None


def update_status(peer_identifier: str, status_value: str, *, use_pane_id: bool = False) -> bool:
    """Update peer status via daemon HTTP API."""
    if use_pane_id:
        payload = {"pane_id": peer_identifier, "status": status_value}
    else:
        payload = {"peer_name": peer_identifier, "status": status_value}
    result = daemon_post("/session/update", payload)
    return result is not None
