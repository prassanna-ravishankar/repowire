"""Tests for ppid-chain pane resolution in hooks._tmux.

Covers the multi-peer-same-cwd case from #107: when TMUX_PANE isn't
inherited by an MCP subprocess, `tmux display-message` would return the
focused pane (wrong peer). ppid-chain matching against `tmux list-panes`
pane_pids resolves unambiguously.
"""

from __future__ import annotations

import subprocess
from unittest.mock import patch

from repowire.hooks import _tmux


def _completed(stdout: str = "", returncode: int = 0) -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=returncode, stdout=stdout, stderr="")


class TestPpidChainResolution:
    def test_matches_ancestor_to_pane(self, monkeypatch):
        monkeypatch.setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
        monkeypatch.delenv("TMUX_PANE", raising=False)

        # Two panes alive, neither focused matches our ancestry — only %2 does
        list_panes = _completed("%1 1000\n%2 2000\n%3 3000\n")

        def fake_run(cmd, **_kw):
            if cmd[:2] == ["tmux", "list-panes"]:
                return list_panes
            if cmd[:3] == ["ps", "-o", "ppid="]:
                pid = int(cmd[-1])
                # ancestry: getppid() -> 5000 -> 2000 -> 1 (stops)
                return _completed({5000: "2000", 2000: "1"}.get(pid, "1"))
            if cmd[:2] == ["tmux", "display-message"]:
                return _completed("%3\n")  # focused pane, should NOT be used
            return _completed("", 1)

        with patch.object(_tmux.os, "getppid", return_value=5000), \
             patch.object(_tmux.subprocess, "run", side_effect=fake_run):
            assert _tmux.get_pane_id() == "%2"

    def test_falls_through_to_display_message_when_no_match(self, monkeypatch):
        """If ppid-chain breaks (e.g. re-parented to init), fall through."""
        monkeypatch.setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
        monkeypatch.delenv("TMUX_PANE", raising=False)

        list_panes = _completed("%1 1000\n%2 2000\n")

        def fake_run(cmd, **_kw):
            if cmd[:2] == ["tmux", "list-panes"]:
                return list_panes
            if cmd[:3] == ["ps", "-o", "ppid="]:
                return _completed("1")  # immediate detach to init
            if cmd[:2] == ["tmux", "display-message"]:
                return _completed("%1\n")
            return _completed("", 1)

        with patch.object(_tmux.os, "getppid", return_value=9999), \
             patch.object(_tmux.subprocess, "run", side_effect=fake_run):
            assert _tmux.get_pane_id() == "%1"

    def test_tmux_pane_env_short_circuits(self, monkeypatch):
        monkeypatch.setenv("TMUX_PANE", "%7")
        assert _tmux.get_pane_id() == "%7"

    def test_not_in_tmux_returns_none(self, monkeypatch):
        monkeypatch.delenv("TMUX_PANE", raising=False)
        monkeypatch.delenv("TMUX", raising=False)
        assert _tmux.get_pane_id() is None

    def test_list_panes_failure_falls_through(self, monkeypatch):
        monkeypatch.setenv("TMUX", "x")
        monkeypatch.delenv("TMUX_PANE", raising=False)

        def fake_run(cmd, **_kw):
            if cmd[:2] == ["tmux", "list-panes"]:
                return _completed("", 1)
            if cmd[:2] == ["tmux", "display-message"]:
                return _completed("%5\n")
            return _completed("", 1)

        with patch.object(_tmux.subprocess, "run", side_effect=fake_run):
            assert _tmux.get_pane_id() == "%5"

    def test_ppid_chain_max_depth(self):
        """Chain walk must terminate even on cycles or deep ancestries."""
        def fake_run(cmd, **_kw):
            if cmd[:3] == ["ps", "-o", "ppid="]:
                # Always return self -> would loop forever without max_depth/seen
                return _completed(cmd[-1])
            return _completed("", 1)

        with patch.object(_tmux.os, "getppid", return_value=100), \
             patch.object(_tmux.subprocess, "run", side_effect=fake_run):
            chain = _tmux._get_ppid_chain(max_depth=8)
            assert len(chain) <= 8
            assert 100 in chain
