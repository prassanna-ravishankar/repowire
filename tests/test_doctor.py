"""Tests for repowire.doctor diagnostic checks."""
from __future__ import annotations

from pathlib import Path
from unittest.mock import MagicMock, patch

import httpx

from repowire.config.models import Config, DaemonConfig, RelayConfig, SpawnSettings
from repowire.doctor import (
    CheckResult,
    Status,
    _worst,
    check_auth_token,
    check_channel_transport,
    check_daemon,
    check_package_manager,
    check_python_version,
    check_relay,
    check_repowire_version,
    check_runtimes,
    check_spawn_allowlist,
    check_state_database,
    check_tmux,
    exit_code,
    run_all,
)

# ---------------------------------------------------------------------------
# Pure check functions
# ---------------------------------------------------------------------------


class TestPythonVersion:
    def test_current_passes(self):
        r = check_python_version()
        assert r.status is Status.OK

    def test_below_min_fails(self):
        # Pick a min higher than any plausible runtime
        r = check_python_version(min_version=(99, 0))
        assert r.status is Status.FAIL


class TestTmux:
    def test_missing_warns(self):
        with patch("repowire.doctor.shutil.which", return_value=None):
            r = check_tmux()
        assert r.status is Status.WARN

    def test_present_ok(self):
        fake_proc = MagicMock(stdout="tmux 3.5a\n")
        with patch("repowire.doctor.shutil.which", return_value="/usr/bin/tmux"), \
             patch("repowire.doctor.subprocess.run", return_value=fake_proc):
            r = check_tmux()
        assert r.status is Status.OK
        assert "3.5a" in r.detail


class TestPackageManager:
    def test_uv_preferred(self):
        def which(tool):
            return "/p/uv" if tool == "uv" else None
        with patch("repowire.doctor.shutil.which", side_effect=which):
            r = check_package_manager()
        assert r.status is Status.OK
        assert r.detail == "uv"

    def test_none_warns(self):
        with patch("repowire.doctor.shutil.which", return_value=None):
            r = check_package_manager()
        assert r.status is Status.WARN


def _mock_client_factory(handler):
    """Return a Client subclass factory that uses an httpx MockTransport."""
    transport = httpx.MockTransport(handler)

    class _MockedClient(httpx.Client):
        def __init__(self, **kwargs):
            kwargs.pop("transport", None)
            kwargs.pop("timeout", None)
            super().__init__(timeout=5.0, transport=transport)

    return _MockedClient


class TestDaemon:
    def test_reachable_ok(self):
        def handler(_request):
            return httpx.Response(
                200, json={"status": "ok", "version": "9.9.9", "relay_mode": False},
            )

        with patch("repowire.doctor.httpx.Client", _mock_client_factory(handler)):
            r = check_daemon("http://localhost:8377")
        assert r.status is Status.OK
        assert "9.9.9" in r.detail

    def test_reachable_reports_degraded_acp_child(self):
        def handler(_request):
            return httpx.Response(
                200,
                json={
                    "status": "ok",
                    "version": "9.9.9",
                    "acp_broker": {
                        "status": "degraded",
                        "enabled": True,
                        "configured_peers": 1,
                        "in_flight": 0,
                        "last_error": "agent-client-protocol SDK not installed",
                    },
                },
            )

        with patch("repowire.doctor.httpx.Client", _mock_client_factory(handler)):
            r = check_daemon("http://localhost:8377")
        assert r.status is Status.FAIL
        child = next(c for c in r.children if c.name == "ACP broker health")
        assert child.status is Status.FAIL
        assert "last_error=agent-client-protocol SDK not installed" in child.detail

    def test_connect_error_fails(self):
        def handler(_request):
            raise httpx.ConnectError("nope")

        with patch("repowire.doctor.httpx.Client", _mock_client_factory(handler)):
            r = check_daemon("http://localhost:8377")
        assert r.status is Status.FAIL


class TestRuntimes:
    def test_no_runtimes_warn(self):
        with patch("repowire.doctor.shutil.which", return_value=None), \
             patch("repowire.doctor.Path.exists", return_value=False):
            r = check_runtimes()
        assert r.status is Status.WARN
        # All children should be SKIP
        assert all(c.status is Status.SKIP for c in r.children)

    def test_claude_hooks_missing_fails(self):
        def which(tool):
            return "/p/claude" if tool == "claude" else None
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_hooks_installed", return_value=False), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=False), \
             patch("repowire.installers.claude_code.get_claude_version", return_value=(2, 1, 80)):
            r = check_runtimes()
        claude_result = next(c for c in r.children if c.name == "claude-code")
        assert claude_result.status is Status.FAIL
        assert r.status is Status.FAIL

    def test_claude_hooks_installed_ok(self):
        def which(tool):
            return "/p/claude" if tool == "claude" else None
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_hooks_installed", return_value=True), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=False), \
             patch("repowire.installers.claude_code.get_claude_version", return_value=(2, 1, 80)):
            r = check_runtimes()
        claude_result = next(c for c in r.children if c.name == "claude-code")
        assert claude_result.status is Status.OK


class TestSpawnConfig:
    def test_unconfigured_skip(self):
        config = Config()
        r = check_spawn_allowlist(config)
        assert r.status is Status.SKIP

    def test_resolved_ok(self, tmp_path: Path):
        config = Config(
            daemon=DaemonConfig(
                spawn=SpawnSettings(
                    commands={"claude-code": "ls"},
                    allowed_paths=[str(tmp_path)],
                ),
            ),
        )
        r = check_spawn_allowlist(config)
        assert r.status is Status.OK

    def test_unresolved_warns(self):
        config = Config(
            daemon=DaemonConfig(
                spawn=SpawnSettings(
                    commands={"claude-code": "definitely-not-a-real-command-xyzzy"},
                    allowed_paths=["/nonexistent/path/xyzzy"],
                ),
            ),
        )
        r = check_spawn_allowlist(config)
        assert r.status is Status.WARN

    def test_command_with_flags(self, tmp_path: Path):
        config = Config(
            daemon=DaemonConfig(
                spawn=SpawnSettings(
                    commands={"claude-code": "ls --color"},
                    allowed_paths=[str(tmp_path)],
                ),
            ),
        )
        r = check_spawn_allowlist(config)
        # First token resolves, so OK
        assert r.status is Status.OK


class TestAuthToken:
    def test_set_ok(self):
        config = Config(daemon=DaemonConfig(auth_token="secret"))
        r = check_auth_token(config)
        assert r.status is Status.OK

    def test_unset_skip(self):
        r = check_auth_token(Config())
        assert r.status is Status.SKIP


class TestStateDatabase:
    def test_legacy_flag_false_still_checks_sqlite_database(self, tmp_path: Path):
        config = Config(experiments={"sqlite_state": False})
        with patch.object(Config, "get_config_dir", return_value=tmp_path):
            r = check_state_database(config)
        assert r.status is Status.WARN
        assert "not initialized" in r.detail

    def test_missing_warns(self, tmp_path: Path):
        with patch.object(Config, "get_config_dir", return_value=tmp_path):
            r = check_state_database(Config())
        assert r.status is Status.WARN
        assert "not initialized" in r.detail

    def test_initialized_ok(self, tmp_path: Path):
        from repowire.daemon.state.database import StateDatabase

        db = StateDatabase(tmp_path / "state.db")
        try:
            db.conn.execute(
                """
                INSERT INTO events(event_id, type, timestamp, payload_json)
                VALUES ('e1', 'test', '2026-05-22T00:00:00+00:00', '{}')
                """,
            )
            db.conn.execute(
                """
                INSERT INTO peer_session_mappings(
                    session_id, display_name, circle, backend, role
                ) VALUES ('p1', 'peer', 'default', 'codex', 'agent')
                """,
            )
            db.conn.commit()
        finally:
            db.close()

        with patch.object(Config, "get_config_dir", return_value=tmp_path):
            r = check_state_database(Config())
        assert r.status is Status.OK
        assert "schema v" in r.detail
        assert "1 event" in r.detail
        assert "1 peer mapping" in r.detail


class TestRelay:
    def test_disabled_skip(self):
        r = check_relay(Config())
        assert r.status is Status.SKIP

    def test_reachable_ok(self):
        config = Config(relay=RelayConfig(enabled=True, url="wss://relay.example", api_key="rw_x"))

        def handler(_request):
            return httpx.Response(200, json={"status": "ok"})

        with patch("repowire.doctor.httpx.Client", _mock_client_factory(handler)):
            r = check_relay(config)
        assert r.status is Status.OK
        assert "API key" in r.detail

    def test_unreachable_fails(self):
        config = Config(relay=RelayConfig(enabled=True, url="wss://relay.example"))

        def handler(_request):
            raise httpx.ConnectError("down")

        with patch("repowire.doctor.httpx.Client", _mock_client_factory(handler)):
            r = check_relay(config)
        assert r.status is Status.FAIL


class TestChannelTransport:
    def test_no_claude_skip(self):
        with patch("repowire.doctor.shutil.which", return_value=None):
            r = check_channel_transport()
        assert r.status is Status.SKIP

    def test_not_configured_skip(self):
        def which(tool):
            return "/p/claude" if tool == "claude" else None
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=False):
            r = check_channel_transport()
        assert r.status is Status.SKIP

    def test_configured_no_bun_fails(self, tmp_path: Path):
        claude_json = tmp_path / ".claude.json"
        claude_json.write_text('{"mcpServers":{"repowire-channel":{}}}')

        def which(tool):
            if tool == "claude":
                return "/p/claude"
            return None
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=True), \
             patch("repowire.installers.claude_code.CLAUDE_JSON", claude_json):
            r = check_channel_transport()
        assert r.status is Status.FAIL

    def test_configured_with_bun_ok(self, tmp_path: Path):
        claude_json = tmp_path / ".claude.json"
        claude_json.write_text('{"mcpServers":{"repowire-channel":{}}}')

        def which(tool):
            return f"/p/{tool}" if tool in ("claude", "bun") else None
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=True), \
             patch("repowire.installers.claude_code.CLAUDE_JSON", claude_json):
            r = check_channel_transport()
        assert r.status is Status.OK

    def test_configured_stale_auth_fails(self, tmp_path: Path):
        claude_json = tmp_path / ".claude.json"
        claude_json.write_text(
            '{"mcpServers":{"repowire-channel":{"env":{"REPOWIRE_AUTH_TOKEN":"old"}}}}',
        )

        def which(tool):
            return f"/p/{tool}" if tool in ("claude", "bun") else None

        config = Config(daemon=DaemonConfig(auth_token="new"))
        with patch("repowire.doctor.shutil.which", side_effect=which), \
             patch("repowire.installers.claude_code.check_channel_installed", return_value=True), \
             patch("repowire.installers.claude_code.CLAUDE_JSON", claude_json):
            r = check_channel_transport(config)
        assert r.status is Status.FAIL
        auth = next(c for c in r.children if c.name == "Channel auth")
        assert auth.status is Status.FAIL
        assert "stale token" in auth.detail


# ---------------------------------------------------------------------------
# Orchestration helpers
# ---------------------------------------------------------------------------


class TestWorstAndExitCode:
    def test_worst_empty(self):
        assert _worst([]) is Status.OK

    def test_worst_picks_fail(self):
        results = [
            CheckResult("a", Status.OK),
            CheckResult("b", Status.WARN),
            CheckResult("c", Status.FAIL),
        ]
        assert _worst(results) is Status.FAIL

    def test_worst_picks_warn(self):
        results = [
            CheckResult("a", Status.OK),
            CheckResult("b", Status.WARN),
            CheckResult("c", Status.SKIP),
        ]
        assert _worst(results) is Status.WARN

    def test_exit_code_zero_when_all_pass(self):
        results = [CheckResult("a", Status.OK), CheckResult("b", Status.WARN)]
        assert exit_code(results) == 0

    def test_exit_code_one_when_fail(self):
        results = [CheckResult("a", Status.OK), CheckResult("b", Status.FAIL)]
        assert exit_code(results) == 1

    def test_exit_code_walks_children(self):
        results = [
            CheckResult(
                "parent",
                Status.WARN,
                children=[CheckResult("child", Status.FAIL)],
            ),
        ]
        assert exit_code(results) == 1


class TestRunAll:
    def test_run_all_returns_list(self):
        config = Config()
        with patch("repowire.doctor.httpx.Client"):  # daemon will fail, that's fine
            results = run_all(config, "http://localhost:8377")
        assert isinstance(results, list)
        assert len(results) >= 8
        names = [r.name for r in results]
        assert "Daemon reachable" in names
        assert "Agent runtimes" in names
        assert "Spawn config" in names


def test_repowire_version_check():
    r = check_repowire_version()
    assert r.status is Status.OK
    assert r.detail  # should have a version string
