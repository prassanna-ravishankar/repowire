#!/usr/bin/env python3
"""Handle the PreToolUse hook - remote tool approval over the mesh.

When ``experiments.remote_tool_approval.enabled`` is on, a gated tool call
(Bash / Edit / Write / ...) is suspended: this hook posts a blocking
choice-question to the daemon and waits for an allow/deny from a human surface
(dashboard / Telegram) or a peer, then returns a PreToolUse
``permissionDecision``. It is the hooks-path analogue of the ACP
ApprovalBroker, sharing the daemon's blocking-question core.

Fail closed everywhere: a daemon that is unavailable, a malformed response, a
timeout, or any unexpected error denies the tool. Ungated tools (and the
experiment being off) fall through with no decision so Claude's native flow is
untouched. This still gates under ``--dangerously-skip-permissions`` because the
decision rides the hook, not Claude's native approval.
"""

from __future__ import annotations

import json
import sys
from typing import Any
from uuid import uuid4

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import daemon_post_with_status, get_display_name

# The single allow option this hook offers. The response must select exactly
# this id to grant; anything else fails closed.
_ALLOW_OPTION_ID = "allow"
# Client HTTP timeout sits above the daemon's wait + a small network margin, but
# must stay below Claude's configured PreToolUse hook timeout (60s in settings).
_CLIENT_TIMEOUT_MARGIN_SECONDS = 5.0
# Cap a tool_input rendered into the prompt so the question stays readable and
# the emitted event payload stays bounded; full (truncated) input rides metadata.
_PROMPT_INPUT_MAX_CHARS = 200


def _allow_decision(reason: str = "") -> dict[str, Any]:
    out: dict[str, Any] = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "allow",
        },
    }
    if reason:
        out["hookSpecificOutput"]["permissionDecisionReason"] = reason
    return out


def _deny_decision(reason: str) -> dict[str, Any]:
    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": reason,
        },
    }


def _summarize_input(tool_input: Any) -> str:
    """Compact one-line rendering of a tool input for the prompt."""
    try:
        text = json.dumps(tool_input, ensure_ascii=False, default=str)
    except (TypeError, ValueError):
        text = str(tool_input)
    text = " ".join(text.split())
    if len(text) > _PROMPT_INPUT_MAX_CHARS:
        text = text[:_PROMPT_INPUT_MAX_CHARS] + "…"
    return text


def main(backend: str = "claude-code") -> int:
    """Entry point for the PreToolUse hook.

    Returns 0 always; the decision (if any) is the printed JSON. A bare exit
    with no decision lets Claude's native permission flow proceed.
    """
    if backend != "claude-code":
        return 0  # PreToolUse remote approval is a Claude Code path only

    # This hook is installed/matched ONLY for gated tools, so once it runs we
    # are about to gate a consequential tool call. Fail closed (deny) on any
    # condition where we cannot affirmatively verify the call is ungated —
    # otherwise empty stdout under --dangerously-skip-permissions is a bypass.
    # The ONLY safe fall-through is: config loads cleanly AND says the tool is
    # not gated (or the experiment is off).
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire pretooluse: invalid JSON input: {e}", file=sys.stderr)
        print(json.dumps(_deny_decision("malformed hook input")))
        return 0

    tool_name = input_data.get("tool_name", "")

    try:
        from repowire.config.models import load_config

        cfg = load_config()
    except Exception as e:  # noqa: BLE001 — can't verify intent → deny, don't bypass
        print(f"repowire pretooluse: config load failed: {e}", file=sys.stderr)
        print(json.dumps(_deny_decision("approval config unavailable")))
        return 0
    approval = cfg.experiments.remote_tool_approval
    if not approval.enabled or tool_name not in approval.gated_tools:
        return 0  # config loaded and says off / ungated: native flow proceeds

    # From here the tool is gated: any failure denies (fail closed).
    tool_input = input_data.get("tool_input", {})
    prompt = f"Allow {tool_name}? {_summarize_input(tool_input)}".rstrip()
    server_wait = approval.timeout_seconds
    client_timeout = server_wait + _CLIENT_TIMEOUT_MARGIN_SECONDS
    correlation_id = f"pretool-{uuid4().hex[:12]}"

    payload = {
        "prompt": prompt,
        "options": [{"id": _ALLOW_OPTION_ID, "title": f"Allow {tool_name}"}],
        "scope": "tool_permission",
        "timeout_seconds": server_wait,
        "correlation_id": correlation_id,
        "origin": "pretooluse",
        "from_peer": get_display_name(),
        "metadata": {
            "tool_name": tool_name,
            "tool_input": _summarize_input(tool_input),
            "session_id": input_data.get("session_id", ""),
            "cwd": input_data.get("cwd", ""),
            "pane_id": get_pane_id() or "",
        },
    }

    status_code, body = daemon_post_with_status(
        "/questions/ask-blocking", payload, timeout=client_timeout,
    )
    if status_code != 200 or not isinstance(body, dict):
        # Daemon unavailable / rejected / malformed: deny.
        detail = f"status={status_code}" if status_code else "no response"
        print(json.dumps(_deny_decision(f"approval unavailable ({detail})")))
        return 0

    outcome = body.get("outcome")
    message = body.get("message") or ""
    # Allow ONLY on the exact allow option this hook offered. A stale / local /
    # malformed response carrying a different option_id (e.g. "deny") must not
    # grant — fail closed.
    if outcome == "answered" and body.get("option_id") == _ALLOW_OPTION_ID:
        print(json.dumps(_allow_decision(message)))
        return 0

    # denied / timed_out / cancelled / acknowledged / no-option → fail closed.
    reason = {
        "timed_out": "approval timed out",
        "denied": "denied by operator",
        "cancelled": "approval cancelled",
    }.get(str(outcome), f"approval not granted ({outcome})")
    if message:
        reason = f"{reason}: {message}"
    print(json.dumps(_deny_decision(reason)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
