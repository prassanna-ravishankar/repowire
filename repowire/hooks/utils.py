"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from contextlib import suppress
from pathlib import Path

from repowire.config.models import DEFAULT_DAEMON_URL

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)


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


def pending_cid_path(pane_id: str) -> Path:
    """Path to the pending correlation_id file for a pane."""
    return pane_logs_dir() / f"pending-{get_pane_file(pane_id)}.json"


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


def clear_pending_cids(pane_id: str | None) -> None:
    """Remove any queued correlation IDs for a pane."""
    if not pane_id:
        return

    pending_path = pending_cid_path(pane_id)
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
    ):
        with suppress(OSError):
            path.unlink()


def _log_daemon_error(method: str, path: str, exc: Exception) -> None:
    """Log daemon request failure, including HTTP response body when available."""
    msg = f"repowire: daemon {method} {path} failed: {exc}"
    if isinstance(exc, urllib.error.HTTPError):
        try:
            body = exc.read().decode(errors="ignore")
            if body:
                msg += f" - Body: {body}"
        except Exception:
            pass
    print(msg, file=sys.stderr)


def daemon_post(path: str, payload: dict, *, timeout: float = 2.0) -> dict | None:
    """POST JSON to daemon. Returns parsed response or None on failure."""
    try:
        data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            f"{DAEMON_URL}{path}",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode())
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
        _log_daemon_error("POST", path, e)
        return None


def daemon_get(path: str, *, timeout: float = 2.0) -> dict | None:
    """GET from daemon. Returns parsed response or None on failure."""
    try:
        req = urllib.request.Request(f"{DAEMON_URL}{path}", method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode())
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
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
