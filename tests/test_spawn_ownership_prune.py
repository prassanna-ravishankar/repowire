"""Prune dead-pane spawn-ownership records + backfill peer_id (repowire-wg4)."""

from __future__ import annotations

from pathlib import Path
from unittest.mock import patch

from repowire.config.models import AgentType
from repowire.protocol.peers import Peer, PeerRole, PeerStatus
from repowire.spawn_ownership import (
    OwnershipValidation,
    TmuxPaneEvidence,
    _load_records,
    backfill_ownership_peer_id,
    prune_dead_ownership,
    record_spawn_ownership,
)


def _seed(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(
        "repowire.spawn_ownership.OWNERSHIP_PATH", tmp_path / "ownership.json"
    )


def _record(pane_id: str, *, peer_id: str | None = None) -> None:
    record_spawn_ownership(
        pane_id=pane_id,
        path="/tmp/proj",
        backend=AgentType.CODEX,
        circle="default",
        role=PeerRole.AGENT,
        display_name="proj-codex",
        tmux_session="sess",
        peer_id=peer_id,
        machine="localhost",
    )


def test_prune_drops_dead_pane_records(monkeypatch, tmp_path: Path) -> None:
    _seed(monkeypatch, tmp_path)
    _record("%1")  # will be live
    _record("%2")  # will be dead

    def fake_probe(pane_id: str):
        if pane_id == "%1":
            return TmuxPaneEvidence(
                pane_id="%1", tmux_session="sess", current_path="/tmp/proj", pane_pid="1234",
            )
        return None  # %2 is dead

    with patch("repowire.spawn_ownership.probe_tmux_pane", side_effect=fake_probe):
        removed = prune_dead_ownership()

    assert removed == 1
    remaining = _load_records()
    assert "%1" in remaining and "%2" not in remaining


def test_prune_is_noop_when_all_live(monkeypatch, tmp_path: Path) -> None:
    _seed(monkeypatch, tmp_path)
    _record("%1")
    evidence = TmuxPaneEvidence(
        pane_id="%1", tmux_session="sess", current_path="/tmp/proj", pane_pid="1234",
    )
    with patch("repowire.spawn_ownership.probe_tmux_pane", return_value=evidence):
        assert prune_dead_ownership() == 0
    assert "%1" in _load_records()


def test_backfill_writes_peer_id_onto_matching_record(monkeypatch, tmp_path: Path) -> None:
    _seed(monkeypatch, tmp_path)
    _record("%1", peer_id=None)  # rehydrated record lost its peer_id

    peer = Peer(
        peer_id="peer-abc",
        display_name="proj-codex",
        path="/tmp/proj",
        machine="localhost",
        backend=AgentType.CODEX,
        circle="default",
        role=PeerRole.AGENT,
        status=PeerStatus.ONLINE,
    )
    evidence = TmuxPaneEvidence(
        pane_id="%1", tmux_session="sess", current_path="/tmp/proj", pane_pid="1234",
    )
    with patch("repowire.spawn_ownership.probe_tmux_pane", return_value=evidence), \
         patch("repowire.spawn_ownership.socket.gethostname", return_value="localhost"):
        updated = backfill_ownership_peer_id(peer)

    assert updated is True
    assert _load_records()["%1"].peer_id == "peer-abc"


def test_backfill_skips_record_that_already_has_peer_id(monkeypatch, tmp_path: Path) -> None:
    _seed(monkeypatch, tmp_path)
    _record("%1", peer_id="existing-id")

    peer = Peer(
        peer_id="peer-abc",
        display_name="proj-codex",
        path="/tmp/proj",
        machine="localhost",
        backend=AgentType.CODEX,
        circle="default",
        role=PeerRole.AGENT,
        status=PeerStatus.ONLINE,
    )
    evidence = TmuxPaneEvidence(
        pane_id="%1", tmux_session="sess", current_path="/tmp/proj", pane_pid="1234",
    )
    with patch("repowire.spawn_ownership.probe_tmux_pane", return_value=evidence), \
         patch("repowire.spawn_ownership.socket.gethostname", return_value="localhost"):
        updated = backfill_ownership_peer_id(peer)

    assert updated is False
    assert _load_records()["%1"].peer_id == "existing-id"  # not overwritten


def test_backfill_uses_prevalidated_match_without_reprobing(monkeypatch, tmp_path: Path) -> None:
    # A caller that already resolved ownership can pass the OwnershipValidation
    # to avoid a second tmux-probing correlation pass.
    _seed(monkeypatch, tmp_path)
    _record("%1", peer_id=None)
    peer = Peer(
        peer_id="peer-abc",
        display_name="proj-codex",
        path="/tmp/proj",
        machine="localhost",
        backend=AgentType.CODEX,
        circle="default",
        role=PeerRole.AGENT,
        status=PeerStatus.ONLINE,
    )
    pre = _load_records()["%1"]
    match = OwnershipValidation(ok=True, record=pre, evidence=None)
    # No probe patch: a re-probe would fail (real tmux), proving the passed
    # match short-circuits the correlation.
    with patch(
        "repowire.spawn_ownership.find_spawn_ownership_for_peer",
        side_effect=AssertionError("must not re-correlate when match is supplied"),
    ):
        updated = backfill_ownership_peer_id(peer, match)
    assert updated is True
    assert _load_records()["%1"].peer_id == "peer-abc"
