"""Tests for peer-registry lazy-repair runtime probes."""

from __future__ import annotations

import subprocess
from datetime import datetime, timezone

from repowire.agent_types import AgentType
from repowire.daemon.registry_repair import has_runtime_evidence
from repowire.protocol.peers import Peer, PeerStatus


def _peer(*, agent_pid: int | None = None, pane_id: str | None = None) -> Peer:
    return Peer(
        peer_id="repow-dev-abc12345",
        display_name="worker",
        path="/tmp/project",
        machine="test",
        backend=AgentType.CLAUDE_CODE,
        circle="dev",
        status=PeerStatus.OFFLINE,
        pane_id=pane_id,
        agent_pid=agent_pid,
        last_seen=datetime.now(timezone.utc),
    )


def test_has_runtime_evidence_accepts_live_agent_pid(monkeypatch):
    calls: list[tuple[int, int]] = []

    def fake_kill(pid: int, signal: int) -> None:
        calls.append((pid, signal))

    monkeypatch.setattr("repowire.daemon.registry_repair.os.kill", fake_kill)

    assert has_runtime_evidence(_peer(agent_pid=12345))
    assert calls == [(12345, 0)]


def test_has_runtime_evidence_ignores_non_positive_agent_pid(monkeypatch):
    calls: list[tuple[int, int]] = []

    def fake_kill(pid: int, signal: int) -> None:
        calls.append((pid, signal))

    monkeypatch.setattr("repowire.daemon.registry_repair.os.kill", fake_kill)

    assert not has_runtime_evidence(_peer(agent_pid=0))
    assert not has_runtime_evidence(_peer(agent_pid=-123))
    assert calls == []


def test_has_runtime_evidence_falls_back_to_live_tmux_pane_without_agent_pid(monkeypatch):
    def fake_run(*args, **kwargs):
        return subprocess.CompletedProcess(args[0], 0, stdout="222\n", stderr="")

    monkeypatch.setattr("repowire.daemon.registry_repair.subprocess.run", fake_run)

    assert has_runtime_evidence(_peer(pane_id="%7"))


def test_has_runtime_evidence_rejects_dead_agent_pid_even_with_live_tmux_pane(
    monkeypatch,
):
    def fake_kill(pid: int, signal: int) -> None:
        raise ProcessLookupError

    def fake_run(*args, **kwargs):
        return subprocess.CompletedProcess(args[0], 0, stdout="222\n", stderr="")

    monkeypatch.setattr("repowire.daemon.registry_repair.os.kill", fake_kill)
    monkeypatch.setattr("repowire.daemon.registry_repair.subprocess.run", fake_run)

    assert not has_runtime_evidence(_peer(agent_pid=12345, pane_id="%7"))


def test_has_runtime_evidence_rejects_missing_pid_and_pane():
    assert not has_runtime_evidence(_peer())


def test_has_runtime_evidence_rejects_dead_tmux_pane(monkeypatch):
    def fake_run(*args, **kwargs):
        return subprocess.CompletedProcess(args[0], 1, stdout="", stderr="no pane")

    monkeypatch.setattr("repowire.daemon.registry_repair.subprocess.run", fake_run)

    assert not has_runtime_evidence(_peer(pane_id="%7"))
