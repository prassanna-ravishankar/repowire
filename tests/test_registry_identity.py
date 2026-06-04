"""Tests for peer-registry identity helpers."""

from __future__ import annotations

from datetime import datetime

from repowire.agent_types import AgentType
from repowire.daemon import peer_registry
from repowire.daemon.registry_identity import (
    SessionMapping,
    is_configured_orchestrator_path,
    normalize_identity_path,
)
from repowire.protocol.peers import PeerRole


def test_session_mapping_coerces_backend_role_and_timestamp():
    mapping = SessionMapping(
        session_id="repow-dev-abc12345",
        display_name="worker",
        circle="dev",
        backend=AgentType.CODEX.value,
        role=PeerRole.ORCHESTRATOR.value,
    )

    assert mapping.backend is AgentType.CODEX
    assert mapping.role is PeerRole.ORCHESTRATOR
    assert mapping.updated_at is not None
    datetime.fromisoformat(mapping.updated_at)


def test_normalize_identity_path_is_reexported_from_peer_registry(tmp_path):
    raw = tmp_path / "folder" / ".." / "folder"
    expected = normalize_identity_path(str(raw))

    assert peer_registry.normalize_identity_path(str(raw)) == expected


def test_configured_orchestrator_path_matches_canonical_path(monkeypatch, tmp_path):
    orchestrator_dir = tmp_path / "orchestrator"
    orchestrator_dir.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(orchestrator_dir, target_is_directory=True)

    monkeypatch.setattr(
        "repowire.orchestrator.workspace.workspace_path",
        lambda: orchestrator_dir,
    )

    assert is_configured_orchestrator_path(str(alias))
    assert not is_configured_orchestrator_path(str(tmp_path / "other"))
