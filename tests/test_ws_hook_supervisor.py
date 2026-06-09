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
