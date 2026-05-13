"""Tests for the ws-hook supervisor (auto-respawn from the Stop hook)."""

from __future__ import annotations

import fcntl
import os
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


def _write_meta(pane_id: str, cwd: str) -> None:
    write_pane_runtime_metadata(
        pane_id,
        {
            "backend": "claude-code",
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
            assert ws_hook_supervisor.maybe_respawn(PANE_ID) is True
            mock_spawn.assert_called_once()
            kwargs = mock_spawn.call_args.kwargs
            assert kwargs["pane_id"] == PANE_ID
            assert kwargs["display_name"] == "respawn-test"
            assert kwargs["backend"] == "claude-code"
            assert kwargs["peer_id"] == "repow-default-deadbeef"
            assert kwargs["cwd"] == str(cache_dir)

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
