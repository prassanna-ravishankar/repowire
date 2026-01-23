"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def update_status(peer_name: str, status: str) -> bool:
    """Update peer status via daemon HTTP API."""
    try:
        data = json.dumps(
            {
                "peer_name": peer_name,
                "status": status,
            }
        ).encode("utf-8")

        req = urllib.request.Request(
            f"{DAEMON_URL}/session/update",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        with urllib.request.urlopen(req, timeout=2.0) as resp:
            return resp.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        print(f"repowire: status update failed for {peer_name}: {e}", file=sys.stderr)
        return False
