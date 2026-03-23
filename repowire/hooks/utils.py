"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

from repowire.config.models import DEFAULT_DAEMON_URL

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)


def get_pane_file(pane_id: str | None) -> str:
    """Normalize pane_id for use in cache filenames (strips % and path separators)."""
    sanitized = (pane_id or "unknown").replace("%", "").replace("/", "").replace("\\", "")
    return sanitized or "unknown"


def get_display_name() -> str:
    """Get display name from env var or cwd folder name."""
    name = os.environ.get("REPOWIRE_DISPLAY_NAME")
    if name:
        return name
    return Path.cwd().name


def derive_display_name(session_id: str | None, cwd: str) -> str:
    """Derive display name from Claude session_id (first 8 chars) or cwd folder name."""
    if session_id:
        return session_id[:8]
    return Path(cwd).name


def pending_cid_path(pane_id: str) -> Path:
    """Path to the pending correlation_id file for a pane."""
    from repowire.config.models import CACHE_DIR

    return CACHE_DIR / "logs" / f"pending-{get_pane_file(pane_id)}.json"


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
