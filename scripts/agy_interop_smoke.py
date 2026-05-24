#!/usr/bin/env python3
"""Record Antigravity (`agy`) interop smoke evidence.

The report is deliberately evidence-oriented: it records what this machine and
daemon currently show, and keeps unverified Antigravity hook/MCP states explicit.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import quote

import httpx

from repowire.installers import antigravity

DEFAULT_DAEMON_URL = "http://127.0.0.1:8377"


@dataclass
class HttpResult:
    ok: bool
    status_code: int | None
    body: Any = None
    error: str | None = None


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _auth_headers() -> dict[str, str]:
    from repowire.config.models import load_config

    try:
        token = load_config().daemon.auth_token
    except Exception:
        token = None
    return {"Authorization": f"Bearer {token}"} if token else {}


def _run(cmd: list[str], *, timeout: float = 10.0) -> dict[str, Any]:
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except FileNotFoundError as exc:
        return {"ok": False, "error": str(exc), "cmd": cmd}
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": f"timed out after {timeout}s", "cmd": cmd}
    return {
        "ok": proc.returncode == 0,
        "returncode": proc.returncode,
        "stdout": proc.stdout.strip(),
        "stderr": proc.stderr.strip(),
        "cmd": cmd,
    }


def _request(
    client: httpx.Client,
    method: str,
    path: str,
    *,
    daemon_url: str,
    json_body: dict[str, Any] | None = None,
    params: dict[str, Any] | None = None,
) -> HttpResult:
    try:
        response = client.request(
            method,
            f"{daemon_url.rstrip('/')}{path}",
            json=json_body,
            params=params,
            headers=_auth_headers(),
        )
    except httpx.HTTPError as exc:
        return HttpResult(ok=False, status_code=None, error=str(exc))

    try:
        body: Any = response.json()
    except ValueError:
        body = response.text
    return HttpResult(
        ok=200 <= response.status_code < 300,
        status_code=response.status_code,
        body=body,
        error=None if 200 <= response.status_code < 300 else str(body),
    )


def collect_agy_cli_evidence() -> dict[str, Any]:
    agy_path = shutil.which("agy")
    evidence: dict[str, Any] = {
        "status": "observed" if agy_path else "not_observed",
        "binary": agy_path,
    }
    if agy_path:
        evidence["changelog"] = _run(["agy", "changelog"], timeout=5.0)
    else:
        evidence["note"] = "`agy` binary not found on PATH"
    return evidence


def collect_plugin_evidence() -> dict[str, Any]:
    plugin_dir = antigravity.PLUGINS_DIR / antigravity.PLUGIN_NAME
    hooks_path = plugin_dir / "hooks" / "hooks.json"
    plugin_json = plugin_dir / "plugin.json"
    manifest = antigravity.MANIFEST_PATH
    installed = antigravity.check_hooks_installed()
    evidence: dict[str, Any] = {
        "status": "observed" if installed else "not_observed",
        "plugin_dir": str(plugin_dir),
        "plugin_json_exists": plugin_json.exists(),
        "hooks_json_exists": hooks_path.exists(),
        "manifest_exists": manifest.exists(),
        "check_hooks_installed": installed,
    }
    if hooks_path.exists():
        try:
            hooks = json.loads(hooks_path.read_text())
            evidence["hook_events_configured"] = sorted(hooks)
        except json.JSONDecodeError as exc:
            evidence["hooks_json_error"] = str(exc)
    if plugin_dir.exists() and shutil.which("agy"):
        evidence["agy_plugin_validate"] = _run(["agy", "plugin", "validate", str(plugin_dir)])
    return evidence


def collect_mcp_evidence() -> dict[str, Any]:
    installed = antigravity.check_mcp_installed()
    return {
        "status": "observed" if installed else "not_verified",
        "check_mcp_installed": installed,
        "note": (
            "Antigravity plugin MCP schema is not verified; installer intentionally "
            "does not write MCP entries."
        ),
    }


def collect_daemon_evidence(client: httpx.Client, daemon_url: str, circle: str) -> dict[str, Any]:
    health = _request(client, "GET", "/health", daemon_url=daemon_url)
    evidence: dict[str, Any] = {
        "status": "observed" if health.ok else "not_observed",
        "health": health.__dict__,
    }
    if not health.ok:
        return evidence

    peers = _request(
        client,
        "GET",
        "/peers",
        daemon_url=daemon_url,
        params={"backend": "antigravity", "circle": circle},
    )
    events = _request(client, "GET", "/events", daemon_url=daemon_url)
    evidence["antigravity_peers"] = peers.__dict__
    evidence["events"] = {
        "ok": events.ok,
        "status_code": events.status_code,
        "count": len(events.body) if isinstance(events.body, list) else None,
    }

    peer_rows = peers.body.get("peers", []) if peers.ok and isinstance(peers.body, dict) else []
    hook_peers = [
        p for p in peer_rows
        if not (p.get("metadata") or {}).get("repowire_cli_fallback")
    ]
    peer_ids = {p.get("peer_id") for p in hook_peers if p.get("peer_id")}
    event_rows = events.body if events.ok and isinstance(events.body, list) else []
    hook_events = [
        e for e in event_rows
        if e.get("peer_id") in peer_ids and e.get("type") in {"chat_turn", "chat_turn_delta"}
    ]
    evidence["hook_firing"] = {
        "status": "observed" if hook_events else "not_observed",
        "non_cli_antigravity_peer_count": len(hook_peers),
        "matching_chat_event_count": len(hook_events),
        "matching_event_ids": [e.get("id") for e in hook_events[-10:]],
        "note": (
            "Observed means daemon state contains non-CLI-fallback antigravity "
            "peer/chat evidence. Not observed is not proof that agy cannot fire hooks."
        ),
    }
    return evidence


def _register_peer(
    client: httpx.Client,
    daemon_url: str,
    *,
    name: str,
    backend: str,
    circle: str,
    metadata: dict[str, Any] | None = None,
) -> HttpResult:
    body = {
        "name": name,
        "path": str(Path.cwd()),
        "backend": backend,
        "circle": circle,
        "machine": socket.gethostname(),
        "metadata": metadata or {},
    }
    return _request(client, "POST", "/peers", daemon_url=daemon_url, json_body=body)


def _delete_peer(client: httpx.Client, daemon_url: str, peer_id: str, circle: str) -> HttpResult:
    return _request(
        client,
        "DELETE",
        f"/peers/{quote(peer_id, safe='')}",
        daemon_url=daemon_url,
        params={"circle": circle},
    )


def _peer_identity(result: HttpResult) -> dict[str, str] | None:
    if not result.ok or not isinstance(result.body, dict):
        return None
    peer_id = result.body.get("peer_id")
    display_name = result.body.get("display_name")
    if not isinstance(peer_id, str) or not isinstance(display_name, str):
        return None
    return {"peer_id": peer_id, "name": display_name}


def _correlation_id(result: HttpResult) -> str | None:
    if not result.ok or not isinstance(result.body, dict):
        return None
    correlation_id = result.body.get("correlation_id")
    return correlation_id if isinstance(correlation_id, str) else None


def run_cli_fallback_smoke(client: httpx.Client, daemon_url: str, circle: str) -> dict[str, Any]:
    stamp = f"{int(time.time())}-{os.getpid()}"
    asker_name = f"agy-smoke-asker-{stamp}"
    recipient_name = f"agy-smoke-peer-{stamp}"
    created: list[dict[str, str]] = []
    correlation_id: str | None = None
    steps: list[dict[str, Any]] = []
    outcome: dict[str, Any] = {"status": "failed", "steps": steps, "cleanup": []}

    def step(name: str, result: HttpResult) -> HttpResult:
        steps.append({"name": name, **result.__dict__})
        return result

    try:
        asker = step(
            "register_asker",
            _register_peer(
                client,
                daemon_url,
                name=asker_name,
                backend="codex",
                circle=circle,
                metadata={"agy_smoke_harness": True},
            ),
        )
        asker_peer = _peer_identity(asker)
        if asker_peer is None:
            return outcome
        created.append(asker_peer)

        recipient = step(
            "register_antigravity_cli_fallback_peer",
            _register_peer(
                client,
                daemon_url,
                name=recipient_name,
                backend="antigravity",
                circle=circle,
                metadata={"repowire_cli_fallback": True, "agy_smoke_harness": True},
            ),
        )
        recipient_peer = _peer_identity(recipient)
        if recipient_peer is None:
            return outcome
        created.append(recipient_peer)

        ask = step(
            "open_ask",
            _request(
                client,
                "POST",
                "/ask",
                daemon_url=daemon_url,
                json_body={
                    "from_peer": asker_peer["peer_id"],
                    "to_peer": recipient_peer["peer_id"],
                    "text": "agy interop smoke ping",
                    "bypass_circle": True,
                },
            ),
        )
        correlation_id = _correlation_id(ask)
        if correlation_id is None:
            return outcome

        pending_before = step(
            "pending_before_ack",
            _request(
                client,
                "GET",
                "/asks/pending",
                daemon_url=daemon_url,
                params={"peer_id": recipient_peer["peer_id"], "direction": "inbound"},
            ),
        )
        saw_pending = (
            pending_before.ok
            and isinstance(pending_before.body, dict)
            and any(
                a.get("correlation_id") == correlation_id
                for a in pending_before.body.get("asks", [])
            )
        )

        ack = step(
            "bare_ack",
            _request(
                client,
                "POST",
                "/ack",
                daemon_url=daemon_url,
                json_body={
                    "correlation_id": correlation_id,
                    "from_peer": recipient_peer["peer_id"],
                },
            ),
        )
        if not ack.ok:
            return outcome

        pending_after = step(
            "pending_after_ack",
            _request(
                client,
                "GET",
                "/asks/pending",
                daemon_url=daemon_url,
                params={"peer_id": recipient_peer["peer_id"], "direction": "inbound"},
            ),
        )
        cleared = (
            pending_after.ok
            and isinstance(pending_after.body, dict)
            and not any(
                a.get("correlation_id") == correlation_id
                for a in pending_after.body.get("asks", [])
            )
        )
        outcome.update({
            "status": "observed" if saw_pending and cleared else "failed",
            "correlation_id": correlation_id,
            "saw_pending_before_ack": saw_pending,
            "cleared_after_ack": cleared,
        })
        return outcome
    finally:
        cleanup: list[dict[str, Any]] = []
        if correlation_id:
            cleanup_ack = _request(
                client,
                "POST",
                "/ack",
                daemon_url=daemon_url,
                json_body={"correlation_id": correlation_id},
            )
            cleanup.append({"name": "cleanup_ack", **cleanup_ack.__dict__})
        for peer in created:
            result = _delete_peer(client, daemon_url, peer["peer_id"], circle)
            cleanup.append({"name": "delete_peer", "peer": peer, **result.__dict__})
        outcome["cleanup"] = cleanup
        if steps:
            steps.append({"name": "cleanup", "results": cleanup})


def build_report(args: argparse.Namespace) -> dict[str, Any]:
    report: dict[str, Any] = {
        "schema_version": 1,
        "generated_at": _now(),
        "daemon_url": args.daemon_url,
        "circle": args.circle,
        "mode": {
            "run_cli_fallback": args.run_cli_fallback,
        },
        "evidence": {
            "agy_cli": collect_agy_cli_evidence(),
            "plugin_layout": collect_plugin_evidence(),
            "mcp_availability": collect_mcp_evidence(),
        },
    }

    with httpx.Client(timeout=args.timeout) as client:
        report["evidence"]["daemon"] = collect_daemon_evidence(
            client, args.daemon_url, args.circle,
        )
        if args.run_cli_fallback:
            report["evidence"]["cli_fallback_ask_ack"] = run_cli_fallback_smoke(
                client, args.daemon_url, args.circle,
            )
        else:
            report["evidence"]["cli_fallback_ask_ack"] = {
                "status": "not_run",
                "note": "Pass --run-cli-fallback to create temporary peers and smoke ask->ack.",
            }
    return report


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--daemon-url",
        default=os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL),
        help=f"Repowire daemon URL (default: {DEFAULT_DAEMON_URL})",
    )
    parser.add_argument("--circle", default="default", help="Circle for daemon peer evidence")
    parser.add_argument(
        "--run-cli-fallback",
        action="store_true",
        help="Create temporary peers and record CLI fallback ask->bare-ack evidence",
    )
    parser.add_argument("--timeout", type=float, default=10.0, help="HTTP timeout in seconds")
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("agy-interop-smoke-report.json"),
        help="JSON report path",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    report = build_report(args)
    args.output.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(f"Wrote {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
