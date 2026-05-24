"""Tests for the session hook handler."""

import json
import signal
from pathlib import Path
from unittest.mock import patch

from repowire.hooks.session_handler import (
    _find_self_peer,
    format_peers_context,
    format_self_context,
    get_peer_name,
    main,
)


class TestFindSelfPeer:
    def test_prefers_peer_id_match(self):
        peers = [
            {"peer_id": "repow-other", "display_name": "alice", "name": "alice"},
            {"peer_id": "repow-me", "display_name": "different-name", "name": "x"},
        ]
        result = _find_self_peer(peers, peer_id="repow-me", display_name="alice")
        assert result is not None
        assert result["peer_id"] == "repow-me"

    def test_falls_back_to_display_name(self):
        peers = [
            {"peer_id": "repow-other", "display_name": "alice", "name": "alice"},
        ]
        result = _find_self_peer(peers, peer_id=None, display_name="alice")
        assert result is not None
        assert result["display_name"] == "alice"

    def test_returns_none_when_not_found(self):
        peers = [{"peer_id": "repow-x", "display_name": "x", "name": "x"}]
        assert _find_self_peer(peers, peer_id="repow-me", display_name="me") is None

    def test_none_peers_list(self):
        assert _find_self_peer(None, peer_id="x", display_name="x") is None


class TestFormatSelfContext:
    def _base_kwargs(self, **overrides):
        kwargs = {
            "display_name": "repowire-claude-code",
            "peer_id": "repow-default-abc12345",
            "circle": "default",
            "circle_source": "tmux",
            "backend": "claude-code",
            "role": None,
            "cwd": "/Users/x/projects/repowire",
            "branch": None,
        }
        kwargs.update(overrides)
        return kwargs

    def test_includes_peer_id_when_assigned(self):
        result = format_self_context(**self._base_kwargs())
        assert "display_name: repowire-claude-code" in result
        assert "peer_id: repow-default-abc12345" in result
        assert "circle: default (from tmux session)" in result
        assert "backend: claude-code" in result
        assert "project: repowire" in result
        assert "path: /Users/x/projects/repowire" in result
        assert "@repowire-claude-code" in result

    def test_omits_peer_id_when_none(self):
        result = format_self_context(**self._base_kwargs(peer_id=None))
        assert "display_name: repowire-claude-code" in result
        assert "peer_id" not in result

    def test_includes_role_when_set(self):
        result = format_self_context(**self._base_kwargs(role="worker"))
        assert "role: worker" in result

    def test_omits_role_when_none(self):
        result = format_self_context(**self._base_kwargs(role=None))
        assert "role:" not in result

    def test_includes_branch_when_set(self):
        result = format_self_context(**self._base_kwargs(branch="feat/x"))
        assert "branch: feat/x" in result

    def test_omits_branch_when_none(self):
        result = format_self_context(**self._base_kwargs(branch=None))
        assert "branch:" not in result

    def test_circle_source_tmux_label(self):
        result = format_self_context(**self._base_kwargs(circle_source="tmux"))
        assert "(from tmux session)" in result

    def test_circle_source_spawn_hint_label(self):
        result = format_self_context(
            **self._base_kwargs(circle="arch", circle_source="spawn_hint"),
        )
        assert "circle: arch (from spawn hint)" in result

    def test_circle_source_fallback_label(self):
        result = format_self_context(
            **self._base_kwargs(circle="default", circle_source="fallback"),
        )
        assert "circle: default (default fallback)" in result

    def test_self_peer_overrides_request_values(self):
        """Daemon-effective values from /peers win over request defaults.

        Covers the main case for this revision: when the daemon has
        restored circle/role from persisted state (or canonicalized the
        display_name), the agent must see those — not the values the
        hook sent at registration time.
        """
        self_peer = {
            "peer_id": "repow-default-effective-id",
            "display_name": "canonical-name",
            "name": "canonical-name",
            "circle": "restored-circle",
            "backend": "codex",
            "role": "worker",
            "path": "/canonical/path/project-x",
            "metadata": {"branch": "feat/restored"},
        }
        result = format_self_context(
            **self._base_kwargs(
                display_name="request-name",
                peer_id="repow-default-request-id",
                circle="request-circle",
                backend="claude-code",
                role=None,
                cwd="/request/path/proj",
                branch="request-branch",
            ),
            self_peer=self_peer,
        )
        assert "canonical-name" in result
        assert "request-name" not in result
        assert "repow-default-effective-id" in result
        assert "repow-default-request-id" not in result
        assert "restored-circle" in result
        assert "request-circle" not in result
        assert "backend: codex" in result
        assert "role: worker" in result
        assert "project: project-x" in result
        assert "path: /canonical/path/project-x" in result
        assert "branch: feat/restored" in result

    def test_self_peer_partial_falls_back_per_field(self):
        """Missing self_peer fields fall back to request values one by one."""
        self_peer = {"peer_id": "repow-x", "display_name": "x", "circle": "circle-from-peer"}
        result = format_self_context(
            **self._base_kwargs(
                display_name="x",
                peer_id="repow-x",
                circle="request-circle",
                backend="claude-code",
                role="request-role",
                branch="request-branch",
            ),
            self_peer=self_peer,
        )
        assert "circle-from-peer" in result
        assert "backend: claude-code" in result
        assert "role: request-role" in result
        assert "branch: request-branch" in result

    def test_circle_source_none_shows_circle_only(self):
        result = format_self_context(**self._base_kwargs(circle_source=None))
        assert "circle: default" in result
        assert "(" not in result.split("circle: default", 1)[1].split("\n", 1)[0]


def _run_with_input(input_data: dict) -> int:
    with patch("sys.stdin") as mock_stdin:
        mock_stdin.read.return_value = json.dumps(input_data)
        return main()


class TestGetPeerName:
    def test_folder_name(self):
        assert get_peer_name("/Users/prass/projects/repowire") == "repowire"

    def test_nested_path(self):
        assert get_peer_name("/a/b/c/myproject") == "myproject"


class TestFormatPeersContext:
    def test_empty_peers(self):
        assert format_peers_context([], "me") == ""

    def test_only_self(self):
        peers = [{"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}}]
        assert format_peers_context(peers, "me") == ""

    def test_formats_online_peers(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {
                "name": "other", "status": "online",
                "path": "/tmp/other", "metadata": {"branch": "main"},
            },
        ]
        result = format_peers_context(peers, "me")
        assert "other" in result
        assert "main" in result
        assert "@dashboard" in result
        assert "set_description" in result

    def test_excludes_offline(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {"name": "offline-peer", "status": "offline", "path": "/tmp/off", "metadata": {}},
        ]
        result = format_peers_context(peers, "me")
        assert result == ""

    def test_shows_description(self):
        peers = [
            {"name": "me", "status": "online", "path": "/tmp/me", "metadata": {}},
            {
                "name": "worker",
                "status": "online",
                "path": "/tmp/worker",
                "metadata": {},
                "description": "fixing auth",
            },
        ]
        result = format_peers_context(peers, "me")
        assert "fixing auth" in result


class TestSessionMain:
    def test_invalid_json(self):
        with patch("sys.stdin") as mock_stdin:
            mock_stdin.read.return_value = "not json"
            assert main() == 0

    def test_session_end_is_noop(self):
        result = _run_with_input({
            "hook_event_name": "SessionEnd",
            "cwd": "/tmp/test",
        })
        assert result == 0

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    def test_session_start_registers(
        self, mock_branch, mock_status, mock_popen, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            mock_register.assert_called_once()
            call_args = mock_register.call_args
            # First positional arg is now path (cwd), not display_name
            assert call_args[0][0] == str(tmp_path)
            assert call_args.kwargs["circle_source"] == "tmux"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": None,
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    def test_session_start_marks_missing_tmux_default_as_fallback(
        self, mock_branch, mock_status, mock_popen, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            call_args = mock_register.call_args
            assert call_args[0][1] == "default"
            assert call_args.kwargs["circle_source"] == "fallback"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": None,
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    def test_session_start_uses_restart_peer_id_hint(
        self, mock_branch, mock_status, mock_popen, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        from repowire.spawn_hints import write_hint

        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            write_hint(
                str(tmp_path),
                "claude-code",
                "default",
                peer_id="repow-default-restart1",
            )
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            call_args = mock_register.call_args
            assert call_args.kwargs["peer_id"] == "repow-default-restart1"
            assert call_args.kwargs["circle_source"] == "spawn_hint"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_second_session_start_skips_ws_hook(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Repeated SessionStart for the same logical session skips ws-hook takeover."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": str(tmp_path),
                "hook_session_id": "eph99999-rest",
                "peer_id": "repow-default-abc12345",
            }))

            # Simulate a held flock — fcntl.flock raises OSError when lock is held
            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": str(tmp_path),
                    "session_id": "eph99999-rest",
                })

                # Should return 0 immediately — ws-hook alive, same project
                assert result == 0
                mock_register.assert_not_called()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "newproj-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_cwd_mismatch_kills_old_ws_hook(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Different cwd in same pane kills old ws-hook and re-registers."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.get_git_branch", return_value=None), \
             patch("repowire.hooks.session_handler.compute_git_status", return_value=None):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": "/old/project",
                "hook_session_id": "old-session",
                "peer_id": "repow-default-old12345",
            }))
            (log_dir / "ws-hook-1.pid").write_text("99999")

            new_cwd = str(tmp_path / "newproj")
            Path(new_cwd).mkdir()

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill, \
                 patch("repowire.hooks.session_handler.subprocess.Popen") as mock_popen:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                # First call (LOCK_NB) fails, second call (blocking) succeeds
                mock_fcntl.flock.side_effect = [
                    OSError("Resource temporarily unavailable"),
                    None,
                ]
                mock_popen.return_value.pid = 12345

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": new_cwd,
                    "session_id": "new-session",
                })

                assert result == 0
                mock_kill.assert_called_once_with(99999, signal.SIGTERM)
                mock_register.assert_called_once()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    def test_same_project_new_session_takes_over(
        self, mock_tmux, mock_register, mock_fetch, tmp_path,
    ):
        """Same cwd with a different hook session_id is treated as a fresh takeover."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.get_git_branch", return_value=None), \
             patch("repowire.hooks.session_handler.compute_git_status", return_value=None):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            (log_dir / "ws-hook-1.meta.json").write_text(json.dumps({
                "backend": "claude-code",
                "cwd": str(tmp_path),
                "hook_session_id": "old-session",
                "peer_id": "repow-default-old12345",
            }))
            (log_dir / "ws-hook-1.pid").write_text("99999")
            (log_dir / "pending-query-1.json").write_text(json.dumps(["stale-cid"]))

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill, \
                 patch("repowire.hooks.session_handler.subprocess.Popen") as mock_popen:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = [
                    OSError("Resource temporarily unavailable"),
                    None,
                ]
                mock_popen.return_value.pid = 12345

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": str(tmp_path),
                    "session_id": "new-session",
                })

                assert result == 0
                mock_kill.assert_called_once_with(99999, signal.SIGTERM)
                mock_register.assert_called_once()
                assert not (log_dir / "pending-query-1.json").exists()

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=("repow-default-abc12345", "test-claude-code", False, True),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.subprocess.Popen")
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=4242)
    def test_session_start_sends_agent_pid_as_ppid_not_own_pid(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_popen,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """The agent_pid in the registration payload must be os.getppid(),
        not os.getpid() — the hook process dies seconds after registration,
        so storing its own pid makes the pane-hijack guard useless after
        the hook exits (issue #190 review)."""
        import os as os_mod

        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler.os.getppid", return_value=31415):
            result = _run_with_input({
                "hook_event_name": "SessionStart",
                "cwd": str(tmp_path),
                "session_id": "abc12345-rest",
            })
            assert result == 0
            mock_register.assert_called_once()
            kwargs = mock_register.call_args.kwargs
            # agent_pid must be the AGENT's pid (the hook's parent), not the
            # hook's own pid. We patched getppid to 31415 — if the value is
            # os.getpid() (this pytest process), the test catches the bug.
            assert kwargs["agent_pid"] == 31415
            assert kwargs["agent_pid"] != os_mod.getpid()
            # parent_pid is computed by walking up one more step from the
            # agent. We mocked _read_ppid_of to 4242.
            assert kwargs["parent_pid"] == 4242
            mock_read_ppid.assert_called_once_with(31415)

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=(None, None, True, False),  # hijack_rejected=True
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=99999)
    def test_rejected_hijack_leaves_incumbent_untouched(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """When the daemon rejects a hijack (409), the hook must NOT kill the
        incumbent's ws-hook, NOT mark the prior peer offline, and NOT clear
        the pane runtime metadata. Issue #190 round-2 review."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler._mark_peer_offline") as mock_offline, \
             patch("repowire.hooks.session_handler.spawn_ws_hook") as mock_spawn, \
             patch("repowire.hooks.session_handler.write_pane_runtime_metadata") as mock_write:
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            incumbent_meta = {
                "backend": "claude-code",
                "cwd": "/incumbent/proj",
                "hook_session_id": "incumbent-session",
                "peer_id": "repow-default-incumbent",
                "display_name": "incumbent-claude-code",
            }
            meta_path = log_dir / "ws-hook-1.meta.json"
            pid_path = log_dir / "ws-hook-1.pid"
            meta_path.write_text(json.dumps(incumbent_meta))
            pid_path.write_text("12345")

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                # Incumbent holds the lock → first flock attempt fails.
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": "/hijacker/proj",
                    "session_id": "hijacker-session",
                })

            assert result == 0
            mock_register.assert_called_once()
            mock_kill.assert_not_called()
            mock_offline.assert_not_called()
            mock_spawn.assert_not_called()
            mock_write.assert_not_called()
            # Incumbent's on-disk state untouched.
            assert meta_path.exists()
            assert json.loads(meta_path.read_text()) == incumbent_meta
            assert pid_path.exists()
            assert pid_path.read_text() == "12345"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=(None, None, False, False),  # transport failure / 5xx: no accept, no 409
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=99999)
    def test_takeover_aborted_when_registration_unconfirmed(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """If the daemon doesn't 409 but also doesn't confirm acceptance
        (transport error, 5xx, malformed body), a pane takeover MUST NOT
        proceed — destroying the incumbent on every daemon hiccup defeats
        the hijack guard. See issue #190 round-3 review."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler._mark_peer_offline") as mock_offline, \
             patch("repowire.hooks.session_handler.spawn_ws_hook") as mock_spawn, \
             patch("repowire.hooks.session_handler.write_pane_runtime_metadata") as mock_write:
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            incumbent_meta = {
                "backend": "claude-code",
                "cwd": "/incumbent/proj",
                "hook_session_id": "incumbent-session",
                "peer_id": "repow-default-incumbent",
                "display_name": "incumbent-claude-code",
            }
            meta_path = log_dir / "ws-hook-1.meta.json"
            pid_path = log_dir / "ws-hook-1.pid"
            meta_path.write_text(json.dumps(incumbent_meta))
            pid_path.write_text("12345")

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": "/newcomer/proj",
                    "session_id": "newcomer-session",
                })

            assert result == 0
            mock_register.assert_called_once()
            mock_kill.assert_not_called()
            mock_offline.assert_not_called()
            mock_spawn.assert_not_called()
            mock_write.assert_not_called()
            assert meta_path.exists()
            assert json.loads(meta_path.read_text()) == incumbent_meta
            assert pid_path.exists()
            assert pid_path.read_text() == "12345"

    @patch("repowire.hooks.session_handler.fetch_peers", return_value=None)
    @patch(
        "repowire.hooks.session_handler._register_peer_http",
        return_value=(
            "repow-default-temp",
            "temp-claude-code",
            False,
            False,
        ),
    )
    @patch(
        "repowire.hooks.session_handler.get_tmux_info",
        return_value={
            "pane_id": "%1",
            "session_name": "default",
            "window_name": "test",
        },
    )
    @patch("repowire.hooks.session_handler.compute_git_status", return_value=None)
    @patch("repowire.hooks.session_handler.get_git_branch", return_value=None)
    @patch("repowire.hooks.session_handler._read_ppid_of", return_value=99999)
    def test_pane_unassigned_registration_leaves_incumbent_untouched(
        self,
        mock_read_ppid,
        mock_branch,
        mock_status,
        mock_tmux,
        mock_register,
        mock_fetch,
        tmp_path,
    ):
        """pane_assigned=False is a non-destructive sticky-pane refusal."""
        with patch("repowire.config.models.CACHE_DIR", tmp_path), \
             patch("repowire.hooks.session_handler._mark_peer_offline") as mock_offline, \
             patch("repowire.hooks.session_handler.clear_pane_runtime_state") as mock_clear, \
             patch("repowire.hooks.session_handler.spawn_ws_hook") as mock_spawn, \
             patch("repowire.hooks.session_handler.write_pane_runtime_metadata") as mock_write:
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            incumbent_meta = {
                "backend": "codex",
                "cwd": "/incumbent/orchestrator",
                "hook_session_id": "orchestrator-session",
                "peer_id": "repow-default-orchestrator",
                "display_name": "orchestrator-codex",
            }
            meta_path = log_dir / "ws-hook-1.meta.json"
            pid_path = log_dir / "ws-hook-1.pid"
            meta_path.write_text(json.dumps(incumbent_meta))
            pid_path.write_text("12345")

            with patch("repowire.hooks.session_handler.fcntl") as mock_fcntl, \
                 patch("repowire.hooks.session_handler.os.kill") as mock_kill:
                mock_fcntl.LOCK_EX = 2
                mock_fcntl.LOCK_NB = 4
                mock_fcntl.flock.side_effect = OSError("Resource temporarily unavailable")

                result = _run_with_input({
                    "hook_event_name": "SessionStart",
                    "cwd": "/temp/project",
                    "session_id": "temp-session",
                })

            assert result == 0
            mock_register.assert_called_once()
            mock_kill.assert_not_called()
            mock_offline.assert_not_called()
            mock_clear.assert_not_called()
            mock_spawn.assert_not_called()
            mock_write.assert_not_called()
            assert json.loads(meta_path.read_text()) == incumbent_meta
            assert pid_path.read_text() == "12345"
