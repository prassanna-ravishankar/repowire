"""Tests for the ws-hook supervisor (auto-respawn from the Stop hook)."""

from __future__ import annotations

import fcntl
import os
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from repowire.hooks import ws_hook_supervisor
from repowire.hooks.utils import (
    write_pane_runtime_metadata,
    ws_hook_lock_path,
    ws_hook_pid_path,
)

PANE_ID = "%99"


@pytest.fixture
def cache_dir(tmp_path, monkeypatch):
    monkeypatch.setattr("repowire.config.models.CACHE_DIR", tmp_path)
    return tmp_path


def _write_meta(pane_id: str, cwd: str, backend: str = "claude-code") -> None:
    write_pane_runtime_metadata(
        pane_id,
        {
            "backend": backend,
            "cwd": cwd,
            "display_name": "respawn-test",
            "hook_session_id": "sess-1",
            "peer_id": "repow-default-deadbeef",
        },
    )


class TestMaybeRespawn:
    def test_no_pane_id_short_circuits(self, cache_dir):
        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(None) is False
            mock_spawn.assert_not_called()

    def test_missing_pid_file_does_not_spawn(self, cache_dir):
        # No pid file written: ws-hook was never started or was cleaned up.
        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
            mock_spawn.assert_not_called()

    def test_skips_when_pid_alive(self, cache_dir):
        # Our own pid is guaranteed alive.
        ws_hook_pid_path(PANE_ID).write_text(str(os.getpid()))
        _write_meta(PANE_ID, str(cache_dir))

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
            mock_spawn.assert_not_called()

    def test_respawns_when_pid_dead_and_lock_free(self, cache_dir):
        # PID 1 (init) cannot be ours, so os.kill(1, 0) raises PermissionError,
        # which counts as alive. Use a sentinel pid that's guaranteed dead.
        ws_hook_pid_path(PANE_ID).write_text("99999999")
        _write_meta(PANE_ID, str(cache_dir))

        with patch.object(
            ws_hook_supervisor, "spawn_ws_hook", return_value=12345,
        ) as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(
                PANE_ID, backend="claude-code", cwd=str(cache_dir),
            ) is True
            mock_spawn.assert_called_once()
            kwargs = mock_spawn.call_args.kwargs
            assert kwargs["pane_id"] == PANE_ID
            assert kwargs["display_name"] == "respawn-test"
            assert kwargs["backend"] == "claude-code"
            assert kwargs["peer_id"] == "repow-default-deadbeef"
            assert kwargs["cwd"] == str(cache_dir)

    def test_rejects_respawn_without_current_context(self, cache_dir, caplog):
        ws_hook_pid_path(PANE_ID).write_text("99999999")
        _write_meta(PANE_ID, str(cache_dir))

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
            mock_spawn.assert_not_called()

        assert "ws-hook respawn rejected" in caplog.text
        assert "metadata claims backend=claude-code" in caplog.text
        assert f"cwd={cache_dir}" in caplog.text
        assert f"(pane {PANE_ID})" in caplog.text

    def test_rejects_respawn_when_backend_mismatches(self, cache_dir, caplog):
        ws_hook_pid_path(PANE_ID).write_text("99999999")
        _write_meta(PANE_ID, str(cache_dir), backend="gemini")

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(
                PANE_ID, backend="claude-code", cwd=str(cache_dir),
            ) is False
            mock_spawn.assert_not_called()

        assert (
            "ws-hook respawn rejected: metadata claims backend=gemini "
            f"cwd={cache_dir} but current hook reports backend=claude-code "
            f"cwd={cache_dir} (pane {PANE_ID})"
        ) in caplog.text

    def test_rejects_respawn_when_cwd_mismatches(self, cache_dir, tmp_path, caplog):
        ws_hook_pid_path(PANE_ID).write_text("99999999")
        metadata_cwd = str(cache_dir / "old")
        current_cwd = str(tmp_path / "current")
        _write_meta(PANE_ID, metadata_cwd)

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(
                PANE_ID, backend="claude-code", cwd=current_cwd,
            ) is False
            mock_spawn.assert_not_called()

        assert (
            "ws-hook respawn rejected: metadata claims backend=claude-code "
            f"cwd={metadata_cwd} but current hook reports backend=claude-code "
            f"cwd={current_cwd} (pane {PANE_ID})"
        ) in caplog.text

    def test_does_not_respawn_when_lock_contested(self, cache_dir):
        ws_hook_pid_path(PANE_ID).write_text("99999999")
        _write_meta(PANE_ID, str(cache_dir))

        # Hold the flock from this test process to simulate another ws-hook
        # owning the pane.
        lock_path = ws_hook_lock_path(PANE_ID)
        contender = open(lock_path, "w")
        try:
            fcntl.flock(contender, fcntl.LOCK_EX)
            with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
                assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
                mock_spawn.assert_not_called()
        finally:
            fcntl.flock(contender, fcntl.LOCK_UN)
            contender.close()

    def test_clears_pid_file_when_metadata_missing(self, cache_dir):
        # Dead pid but no usable metadata -- can't reconstruct the connect
        # payload, so drop the stale pid file and stay out of the way.
        pid_path = ws_hook_pid_path(PANE_ID)
        pid_path.write_text("99999999")

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
            mock_spawn.assert_not_called()
            assert not pid_path.exists()

    def test_garbled_pid_file_does_not_spawn(self, cache_dir):
        ws_hook_pid_path(PANE_ID).write_text("not-a-number")
        _write_meta(PANE_ID, str(cache_dir))

        with patch.object(ws_hook_supervisor, "spawn_ws_hook") as mock_spawn:
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is False
            mock_spawn.assert_not_called()


class TestSpawnWsHook:
    def test_passes_agent_pid_to_child_env(self, cache_dir):
        lock_path = ws_hook_lock_path(PANE_ID)
        lock_fd = open(lock_path, "w")  # noqa: SIM115
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX)
            with patch.object(
                ws_hook_supervisor.subprocess,
                "Popen",
                return_value=SimpleNamespace(pid=12345),
            ) as popen:
                pid = ws_hook_supervisor.spawn_ws_hook(
                    pane_id=PANE_ID,
                    peer_id="repow-default-deadbeef",
                    display_name="lifetime-test",
                    backend="claude-code",
                    cwd=str(cache_dir),
                    lock_fd=lock_fd,
                    agent_pid=67890,
                )

            assert pid == 12345
            env = popen.call_args.kwargs["env"]
            assert env["REPOWIRE_AGENT_PID"] == "67890"
            assert env["REPOWIRE_PEER_ID"] == "repow-default-deadbeef"
            assert env["TMUX_PANE"] == PANE_ID
        finally:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
            lock_fd.close()


class TestSweepOrphanWsHooks:
    """Startup sweep: kill ws-hooks whose agent is conclusively gone."""

    def _arm(self, monkeypatch, *, procs, panes, owned_pids=None):
        """Wire the sweep's probes: live processes, live panes, pid files."""
        monkeypatch.setattr(
            ws_hook_supervisor, "_list_ws_hook_pids", lambda: procs
        )
        monkeypatch.setattr(
            ws_hook_supervisor, "_live_tmux_panes", lambda: panes
        )
        for pid, pane_id in (owned_pids or {}).items():
            ws_hook_pid_path(pane_id).write_text(str(pid))

    def test_unowned_hook_is_swept(self, cache_dir, monkeypatch):
        self._arm(monkeypatch, procs={4242: "python websocket_hook.py"}, panes={})
        killed: list[int] = []
        monkeypatch.setattr(
            ws_hook_supervisor, "_terminate", lambda pid: killed.append(pid) or True
        )
        reports = ws_hook_supervisor.sweep_orphan_ws_hooks()
        assert killed == [4242]
        assert reports[0].reason.startswith("no pid file")
        assert reports[0].killed is True

    def test_dead_pane_hook_is_swept_with_peer_id(self, cache_dir, monkeypatch):
        _write_meta("%7", cwd="/tmp/x")
        self._arm(
            monkeypatch,
            procs={4242: "python websocket_hook.py"},
            panes={},  # tmux answered: no panes exist
            owned_pids={4242: "%7"},
        )
        monkeypatch.setattr(ws_hook_supervisor, "_terminate", lambda pid: True)
        reports = ws_hook_supervisor.sweep_orphan_ws_hooks()
        assert len(reports) == 1
        assert reports[0].pane_id == "%7"
        assert reports[0].peer_id == "repow-default-deadbeef"
        assert "no longer exists" in reports[0].reason
        # Stale pid file cleaned up.
        assert not ws_hook_pid_path("%7").exists()

    def test_dead_agent_pid_is_swept(self, cache_dir, monkeypatch):
        write_pane_runtime_metadata(
            "%7",
            {"peer_id": "repow-default-deadbeef", "agent_pid": 999_999_999},
        )
        self._arm(
            monkeypatch,
            procs={4242: "python websocket_hook.py"},
            panes={"%7": 200},
            owned_pids={4242: "%7"},
        )
        monkeypatch.setattr(ws_hook_supervisor, "_terminate", lambda pid: True)
        reports = ws_hook_supervisor.sweep_orphan_ws_hooks()
        assert len(reports) == 1
        assert "agent pid 999999999 is dead" in reports[0].reason

    def test_shell_only_subtree_is_swept(self, cache_dir, monkeypatch):
        """The classic orphan: pane and shell alive, agent quit."""
        write_pane_runtime_metadata(
            "%7",
            {"peer_id": "repow-default-deadbeef", "agent_pid": os.getpid()},
        )
        self._arm(
            monkeypatch,
            procs={4242: "python websocket_hook.py"},
            panes={"%7": 200},
            owned_pids={4242: "%7"},
        )
        monkeypatch.setattr(
            "repowire.hooks.websocket_hook._build_ps_child_map",
            lambda: ({200: []}, {200: "zsh"}),
        )
        monkeypatch.setattr(ws_hook_supervisor, "_terminate", lambda pid: True)
        reports = ws_hook_supervisor.sweep_orphan_ws_hooks()
        assert len(reports) == 1
        assert "only shells" in reports[0].reason

    def test_hook_serving_live_agent_is_untouched(self, cache_dir, monkeypatch):
        write_pane_runtime_metadata(
            "%7",
            {"peer_id": "repow-default-deadbeef", "agent_pid": os.getpid()},
        )
        self._arm(
            monkeypatch,
            procs={4242: "python websocket_hook.py"},
            panes={"%7": 200},
            owned_pids={4242: "%7"},
        )
        monkeypatch.setattr(
            "repowire.hooks.websocket_hook._build_ps_child_map",
            lambda: ({200: [300]}, {200: "zsh", 300: "claude"}),
        )
        killed: list[int] = []
        monkeypatch.setattr(
            ws_hook_supervisor, "_terminate", lambda pid: killed.append(pid) or True
        )
        assert ws_hook_supervisor.sweep_orphan_ws_hooks() == []
        assert killed == []

    def test_inconclusive_tmux_listing_never_kills(self, cache_dir, monkeypatch):
        _write_meta("%7", cwd="/tmp/x")
        self._arm(
            monkeypatch,
            procs={4242: "python websocket_hook.py"},
            panes=None,  # tmux listing failed: unknowable
            owned_pids={4242: "%7"},
        )
        killed: list[int] = []
        monkeypatch.setattr(
            ws_hook_supervisor, "_terminate", lambda pid: killed.append(pid) or True
        )
        assert ws_hook_supervisor.sweep_orphan_ws_hooks() == []
        assert killed == []

    def test_dry_run_reports_without_killing(self, cache_dir, monkeypatch):
        self._arm(monkeypatch, procs={4242: "python websocket_hook.py"}, panes={})
        killed: list[int] = []
        monkeypatch.setattr(
            ws_hook_supervisor, "_terminate", lambda pid: killed.append(pid) or True
        )
        reports = ws_hook_supervisor.sweep_orphan_ws_hooks(dry_run=True)
        assert len(reports) == 1
        assert reports[0].killed is False
        assert killed == []
