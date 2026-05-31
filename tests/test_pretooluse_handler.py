"""PreToolUse remote-approval hook handler (repowire-qnp).

The hook is fail-closed: it only ever ALLOWS on an explicit answered+option,
and DENIES on timeout / deny / daemon-unavailable / malformed. Ungated tools
and the experiment being off fall through with no decision (empty stdout) so
Claude's native flow is untouched.
"""

from __future__ import annotations

import io
import json

from repowire.config.models import Config
from repowire.hooks import pretooluse_handler


def _run(monkeypatch, *, input_data, config, post_result=(200, None)):
    """Drive main() with mocked stdin / config / daemon, return parsed stdout."""
    monkeypatch.setattr("sys.stdin", io.StringIO(json.dumps(input_data)))
    monkeypatch.setattr(pretooluse_handler, "get_display_name", lambda: "agent-x")
    monkeypatch.setattr(pretooluse_handler, "get_pane_id", lambda: "%1")
    monkeypatch.setattr(
        "repowire.config.models.load_config", lambda: config,
    )
    posted: dict = {}

    def fake_post(path, payload, *, timeout):
        posted["path"] = path
        posted["payload"] = payload
        posted["timeout"] = timeout
        return post_result

    monkeypatch.setattr(pretooluse_handler, "daemon_post_with_status", fake_post)

    captured = io.StringIO()
    monkeypatch.setattr("sys.stdout", captured)
    rc = pretooluse_handler.main(backend="claude-code")
    assert rc == 0  # the hook never blocks the tool by exit code; decision is in stdout
    out = captured.getvalue().strip()
    decision = json.loads(out) if out else None
    return decision, posted


def _enabled_config() -> Config:
    cfg = Config()
    cfg.experiments.remote_tool_approval.enabled = True
    return cfg


def _decision(decision) -> str | None:
    return decision["hookSpecificOutput"]["permissionDecision"] if decision else None


def test_experiment_off_falls_through(monkeypatch):
    decision, posted = _run(
        monkeypatch, input_data={"tool_name": "Bash"}, config=Config(),
    )
    assert decision is None  # no decision: native flow proceeds
    assert posted == {}  # daemon not contacted


def test_ungated_tool_falls_through(monkeypatch):
    decision, posted = _run(
        monkeypatch,
        input_data={"tool_name": "Read", "tool_input": {"file": "x"}},
        config=_enabled_config(),
    )
    assert decision is None
    assert posted == {}


def test_allow_on_answered_option(monkeypatch):
    decision, posted = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {"command": "ls"}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "answered", "option_id": "allow", "message": "ok"}),
    )
    assert _decision(decision) == "allow"
    assert posted["path"] == "/questions/ask-blocking"
    assert posted["payload"]["scope"] == "tool_permission"
    assert posted["payload"]["correlation_id"].startswith("pretool-")
    # Client timeout sits above the server wait.
    assert posted["timeout"] > posted["payload"]["timeout_seconds"]


def test_deny_on_denied_outcome(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Write", "tool_input": {}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "denied", "message": "nope"}),
    )
    assert _decision(decision) == "deny"


def test_deny_on_timeout(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Edit", "tool_input": {}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "timed_out"}),
    )
    assert _decision(decision) == "deny"


def test_deny_on_acknowledged_fail_closed(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "acknowledged"}),
    )
    assert _decision(decision) == "deny"


def test_deny_on_answered_without_option_fail_closed(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "answered"}),  # no option_id
    )
    assert _decision(decision) == "deny"


def test_deny_on_daemon_unavailable(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {}},
        config=_enabled_config(),
        post_result=(None, None),  # transport failure
    )
    assert _decision(decision) == "deny"


def test_deny_on_non_200(monkeypatch):
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {}},
        config=_enabled_config(),
        post_result=(503, {"detail": "busy"}),
    )
    assert _decision(decision) == "deny"


def test_prompt_input_is_truncated(monkeypatch):
    big = {"command": "x" * 1000}
    decision, posted = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": big},
        config=_enabled_config(),
        post_result=(200, {"outcome": "answered", "option_id": "allow"}),
    )
    assert len(posted["payload"]["prompt"]) < 400  # bounded, not the full 1000 chars
    assert _decision(decision) == "allow"


def test_deny_on_wrong_option_id_fail_closed(monkeypatch):
    # A stale / local / malformed response selecting a different option must
    # not allow, even with outcome=answered.
    decision, _ = _run(
        monkeypatch,
        input_data={"tool_name": "Bash", "tool_input": {}},
        config=_enabled_config(),
        post_result=(200, {"outcome": "answered", "option_id": "deny"}),
    )
    assert _decision(decision) == "deny"


def test_deny_on_malformed_input(monkeypatch):
    # The hook only runs for gated tools; bad JSON means we can't verify, so deny
    # rather than fall through (which would bypass under skip-permissions).
    monkeypatch.setattr("sys.stdin", io.StringIO("not json{"))
    captured = io.StringIO()
    monkeypatch.setattr("sys.stdout", captured)
    rc = pretooluse_handler.main(backend="claude-code")
    assert rc == 0
    decision = json.loads(captured.getvalue().strip())
    assert decision["hookSpecificOutput"]["permissionDecision"] == "deny"


def test_deny_on_config_load_failure(monkeypatch):
    monkeypatch.setattr("sys.stdin", io.StringIO(json.dumps({"tool_name": "Bash"})))

    def boom():
        raise RuntimeError("config exploded")

    monkeypatch.setattr("repowire.config.models.load_config", boom)
    captured = io.StringIO()
    monkeypatch.setattr("sys.stdout", captured)
    rc = pretooluse_handler.main(backend="claude-code")
    assert rc == 0
    decision = json.loads(captured.getvalue().strip())
    assert decision["hookSpecificOutput"]["permissionDecision"] == "deny"


def test_non_claude_backend_falls_through(monkeypatch):
    monkeypatch.setattr("sys.stdin", io.StringIO(json.dumps({"tool_name": "Bash"})))
    captured = io.StringIO()
    monkeypatch.setattr("sys.stdout", captured)
    rc = pretooluse_handler.main(backend="codex")
    assert rc == 0
    assert captured.getvalue().strip() == ""
