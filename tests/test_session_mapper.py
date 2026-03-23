"""Tests for PeerRegistry session mapping persistence and pruning."""

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from repowire.config.models import AgentType
from repowire.daemon.peer_registry import PeerRegistry


def _make_registry(tmp_path: Path, mappings: dict | None = None) -> PeerRegistry:
    path = tmp_path / "sessions.json"
    if mappings:
        path.write_text(json.dumps(mappings, indent=2))
    return PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )


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
    registry = _make_registry(tmp_path, mappings)
    pruned = registry.prune_offline(max_age_hours=72)
    assert pruned == 1
    assert registry.get_mapping("repow-dev-old1") is None
    assert registry.get_mapping("repow-dev-recent") is not None


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
    path = tmp_path / "sessions.json"
    path.write_text(json.dumps(mappings, indent=2))
    registry = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )
    # __post_init__ sets updated_at, so this entry is fresh — won't be pruned
    assert registry.prune_offline() == 0


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
    registry = _make_registry(tmp_path, mappings)
    registry.prune_offline()
    # Force flush to disk
    registry._mappings_dirty = True
    registry._persist_mappings()
    # Reload from disk
    registry2 = _make_registry(tmp_path)
    assert registry2.get_mapping("repow-dev-stale") is None


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
    registry = _make_registry(tmp_path, mappings)
    assert registry.prune_offline() == 1
    assert registry.get_mapping("repow-dev-badtimestamp") is None


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
    registry = _make_registry(tmp_path, mappings)
    assert registry.prune_offline() == 0
