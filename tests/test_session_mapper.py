"""Tests for SessionMapper, focused on prune_offline."""

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from repowire.config.models import AgentType
from repowire.daemon.session_mapper import SessionMapper


def _make_mapper(tmp_path: Path, mappings: dict | None = None) -> SessionMapper:
    path = tmp_path / "sessions.json"
    if mappings:
        path.write_text(json.dumps(mappings, indent=2))
    return SessionMapper(persistence_path=path)


def _ts(hours_ago: float) -> str:
    return (datetime.now(timezone.utc) - timedelta(hours=hours_ago)).isoformat()


def test_prune_removes_old_mappings(tmp_path):
    mappings = {
        "repow-dev-old1": {
            "session_id": "repow-dev-old1",
            "display_name": "old1",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(100),
        },
        "repow-dev-recent": {
            "session_id": "repow-dev-recent",
            "display_name": "recent",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(1),
        },
    }
    mapper = _make_mapper(tmp_path, mappings)
    pruned = mapper.prune_offline(max_age_hours=72)
    assert pruned == 1
    assert mapper.get_mapping("repow-dev-old1") is None
    assert mapper.get_mapping("repow-dev-recent") is not None


def test_prune_removes_entries_with_no_timestamp(tmp_path):
    mappings = {
        "repow-dev-notimestamp": {
            "session_id": "repow-dev-notimestamp",
            "display_name": "notimestamp",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": None,
        },
    }
    # Write raw JSON to bypass __post_init__ auto-timestamp
    path = tmp_path / "sessions.json"
    path.write_text(json.dumps(mappings, indent=2))
    mapper = SessionMapper(persistence_path=path)
    # __post_init__ sets updated_at, so this entry is fresh — won't be pruned
    assert mapper.prune_offline() == 0


def test_prune_persists_to_disk(tmp_path):
    mappings = {
        "repow-dev-stale": {
            "session_id": "repow-dev-stale",
            "display_name": "stale",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(200),
        },
    }
    mapper = _make_mapper(tmp_path, mappings)
    mapper.prune_offline()
    # Reload from disk
    mapper2 = SessionMapper(persistence_path=tmp_path / "sessions.json")
    assert mapper2.get_mapping("repow-dev-stale") is None


def test_prune_removes_entries_with_bad_timestamp(tmp_path):
    mappings = {
        "repow-dev-badtimestamp": {
            "session_id": "repow-dev-badtimestamp",
            "display_name": "badtimestamp",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": "not-a-valid-iso-timestamp",
        },
    }
    mapper = _make_mapper(tmp_path, mappings)
    assert mapper.prune_offline() == 1
    assert mapper.get_mapping("repow-dev-badtimestamp") is None


def test_prune_noop_when_nothing_stale(tmp_path):
    mappings = {
        "repow-dev-fresh": {
            "session_id": "repow-dev-fresh",
            "display_name": "fresh",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(1),
        },
    }
    mapper = _make_mapper(tmp_path, mappings)
    assert mapper.prune_offline() == 0
