"""Durable proof that a tmux pane was created by Repowire spawn.

The peer/session mapping is identity state, not ownership proof. This module
keeps the destructive-action proof separate and revocable: records are written
only after Repowire creates a tmux pane, and are removed when that pane dies or
is intentionally killed.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Literal

from repowire.config.models import AgentType, Config
from repowire.protocol.peers import Peer, PeerRole

OWNERSHIP_PATH = Config.get_config_dir() / "spawn_ownership.json"

OwnershipError = Literal[
    "missing_ownership",
    "ambiguous_ownership",
    "ownership_machine_mismatch",
    "ownership_peer_mismatch",
    "ownership_identity_mismatch",
    "pane_not_live",
    "pane_identity_mismatch",
]


@dataclass
class SpawnOwnershipRecord:
    """A revocable pane ownership record written by daemon spawn."""

    pane_id: str
    path: str
    backend: str
    circle: str
    role: str
    display_name: str
    tmux_session: str
    machine: str
    peer_id: str | None = None
    created_at: float = 0.0

    def __post_init__(self) -> None:
        self.path = _norm_path(self.path)
        if not self.created_at:
            self.created_at = time.time()


@dataclass
class TmuxPaneEvidence:
    """Live tmux evidence for a pane id."""

    pane_id: str
    tmux_session: str
    current_path: str
    pane_pid: str


@dataclass
class OwnershipValidation:
    """Result of validating a durable ownership record."""

    ok: bool
    record: SpawnOwnershipRecord | None = None
    evidence: TmuxPaneEvidence | None = None
    error: OwnershipError | None = None
    hint: str = ""


def record_spawn_ownership(
    *,
    pane_id: str,
    path: str,
    backend: AgentType | str,
    circle: str,
    role: PeerRole | str,
    display_name: str,
    tmux_session: str,
    peer_id: str | None = None,
    machine: str | None = None,
) -> None:
    """Persist proof that Repowire spawned ``pane_id``.

    This is intentionally keyed by pane id, but validation also checks the
    identity tuple and live tmux evidence so a stale/reused pane id is not enough
    to authorize a kill.
    """

    if not pane_id:
        return
    record = SpawnOwnershipRecord(
        pane_id=pane_id,
        path=path,
        backend=_enum_value(backend),
        circle=circle,
        role=_enum_value(role),
        display_name=display_name,
        tmux_session=tmux_session,
        machine=machine or socket.gethostname(),
        peer_id=peer_id,
    )
    records = _load_records()
    records[pane_id] = record
    _save_records(records)


def forget_spawn_ownership(pane_id: str) -> None:
    """Remove any durable spawn ownership proof for ``pane_id``."""

    if not pane_id:
        return
    records = _load_records()
    if records.pop(pane_id, None) is not None:
        _save_records(records)


def get_spawn_ownership(pane_id: str) -> SpawnOwnershipRecord | None:
    """Return the durable ownership record for ``pane_id``, if present."""

    return _load_records().get(pane_id)


def find_spawn_ownership_for_peer(peer: Peer) -> OwnershipValidation:
    """Find unique durable pane ownership proof for a peer identity.

    Used after daemon restart when the live runtime may still be in tmux but did
    not re-run SessionStart, so the rehydrated peer record lacks pane fields.
    This never infers ownership from mappings alone: a matching durable proof
    must also have live tmux evidence for its recorded pane.
    """

    self_machine = socket.gethostname()
    matches: list[OwnershipValidation] = []
    saw_identity_match = False
    saw_dead_pane = False
    saw_mismatch = False

    for record in _load_records().values():
        if record.machine and record.machine != self_machine:
            continue
        if record.peer_id and record.peer_id != peer.peer_id:
            continue
        if not _record_matches_peer_identity(record, peer):
            continue

        saw_identity_match = True
        evidence = probe_tmux_pane(record.pane_id)
        if evidence is None:
            saw_dead_pane = True
            continue
        if (
            evidence.tmux_session != record.tmux_session
            or _norm_path(evidence.current_path) != _norm_path(record.path)
        ):
            saw_mismatch = True
            continue
        matches.append(OwnershipValidation(ok=True, record=record, evidence=evidence))

    if len(matches) == 1:
        return matches[0]
    if len(matches) > 1:
        return OwnershipValidation(
            ok=False,
            error="ambiguous_ownership",
            hint=(
                "Multiple durable Repowire spawn ownership proofs match this peer "
                "identity; refusing to guess which pane to kill."
            ),
        )
    if saw_dead_pane:
        return OwnershipValidation(
            ok=False,
            error="pane_not_live",
            hint="The recorded pane is not visible in tmux; refusing to use stale proof.",
        )
    if saw_mismatch:
        return OwnershipValidation(
            ok=False,
            error="pane_identity_mismatch",
            hint="Live tmux pane evidence does not match the ownership proof.",
        )
    if saw_identity_match:
        return OwnershipValidation(
            ok=False,
            error="missing_ownership",
            hint="No live durable Repowire spawn ownership proof matches this peer.",
        )
    return OwnershipValidation(
        ok=False,
        error="missing_ownership",
        hint=(
            "No durable Repowire spawn ownership proof exists for this peer identity. "
            "Manual peers and pre-proof spawns are left untouched."
        ),
    )


def validate_spawn_ownership(peer: Peer) -> OwnershipValidation:
    """Validate that ``peer`` still corresponds to a daemon-spawned live pane."""

    pane_id = peer.pane_id
    if not pane_id:
        return OwnershipValidation(
            ok=False,
            error="missing_ownership",
            hint="Peer has no pane id, so Repowire cannot prove a pane to kill.",
        )

    record = get_spawn_ownership(pane_id)
    if record is None:
        return OwnershipValidation(
            ok=False,
            error="missing_ownership",
            hint=(
                "No durable Repowire spawn ownership proof exists for this pane. "
                "Manual peers and pre-proof spawns are left untouched."
            ),
        )

    self_machine = socket.gethostname()
    if record.machine and record.machine != self_machine:
        return OwnershipValidation(
            ok=False,
            record=record,
            error="ownership_machine_mismatch",
            hint="Ownership proof was written on a different host.",
        )

    if record.peer_id and record.peer_id != peer.peer_id:
        return OwnershipValidation(
            ok=False,
            record=record,
            error="ownership_peer_mismatch",
            hint="Ownership proof belongs to a different peer id.",
        )

    if (
        record.backend != _enum_value(peer.backend)
        or record.circle != (peer.circle or "default")
        or record.role != _enum_value(peer.role)
        or _norm_path(record.path) != _norm_path(peer.path)
    ):
        return OwnershipValidation(
            ok=False,
            record=record,
            error="ownership_identity_mismatch",
            hint="Ownership proof no longer matches the peer backend/path/circle/role.",
        )

    evidence = probe_tmux_pane(pane_id)
    if evidence is None:
        return OwnershipValidation(
            ok=False,
            record=record,
            error="pane_not_live",
            hint="The recorded pane is not visible in tmux; refusing to use stale proof.",
        )

    if (
        evidence.tmux_session != record.tmux_session
        or _norm_path(evidence.current_path) != _norm_path(record.path)
    ):
        return OwnershipValidation(
            ok=False,
            record=record,
            evidence=evidence,
            error="pane_identity_mismatch",
            hint="Live tmux pane evidence does not match the ownership proof.",
        )

    return OwnershipValidation(ok=True, record=record, evidence=evidence)


def _record_matches_peer_identity(record: SpawnOwnershipRecord, peer: Peer) -> bool:
    return (
        record.backend == _enum_value(peer.backend)
        and record.circle == (peer.circle or "default")
        and record.role == _enum_value(peer.role)
        and _norm_path(record.path) == _norm_path(peer.path)
    )


def probe_tmux_pane(pane_id: str) -> TmuxPaneEvidence | None:
    """Return live tmux evidence for ``pane_id`` or None when unavailable."""

    if not pane_id:
        return None
    try:
        result = subprocess.run(
            [
                "tmux",
                "display-message",
                "-t",
                pane_id,
                "-p",
                "#{session_name}\t#{window_name}\t#{pane_current_path}\t#{pane_pid}",
            ],
            capture_output=True,
            text=True,
            timeout=2,
            check=False,
        )
    except (FileNotFoundError, OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    parts = result.stdout.rstrip("\n").split("\t")
    if len(parts) != 4 or not parts[0] or not parts[1] or not parts[3]:
        return None
    return TmuxPaneEvidence(
        pane_id=pane_id,
        tmux_session=f"{parts[0]}:{parts[1]}",
        current_path=parts[2],
        pane_pid=parts[3],
    )


def _load_records() -> dict[str, SpawnOwnershipRecord]:
    try:
        raw = OWNERSHIP_PATH.read_text()
    except OSError:
        return {}
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    if not isinstance(data, dict):
        return {}
    records: dict[str, SpawnOwnershipRecord] = {}
    for pane_id, payload in data.items():
        if not isinstance(pane_id, str) or not isinstance(payload, dict):
            continue
        try:
            record = SpawnOwnershipRecord(**payload)
        except (TypeError, ValueError):
            continue
        if record.pane_id == pane_id:
            records[pane_id] = record
    return records


def _save_records(records: dict[str, SpawnOwnershipRecord]) -> None:
    OWNERSHIP_PATH.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = OWNERSHIP_PATH.with_suffix(".json.tmp")
    data: dict[str, dict[str, Any]] = {
        pane_id: asdict(record) for pane_id, record in records.items()
    }
    tmp_path.write_text(json.dumps(data, indent=2, sort_keys=True))
    os.replace(str(tmp_path), str(OWNERSHIP_PATH))


def _norm_path(path: str | None) -> str:
    if not path:
        return ""
    return os.path.realpath(str(Path(path).expanduser()))


def _enum_value(value: AgentType | PeerRole | str) -> str:
    if hasattr(value, "value"):
        return str(value.value)
    return str(value)
