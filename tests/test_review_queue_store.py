"""Tests for ReviewQueueStore (atomic JSON persistence)."""

from __future__ import annotations

import json

from repowire.daemon.review_queue_store import ReviewQueueStore


def test_upsert_creates_entry(tmp_path):
    store = ReviewQueueStore(tmp_path / "rq.json")
    entry = store.upsert("alice", "https://github.com/o/r/pull/1", "abc123")
    assert entry.pr_url == "https://github.com/o/r/pull/1"
    assert entry.last_reviewed_sha == "abc123"
    listed = store.list_for("alice")
    assert len(listed) == 1
    assert listed[0].pr_url == entry.pr_url


def test_upsert_updates_existing(tmp_path):
    store = ReviewQueueStore(tmp_path / "rq.json")
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc123")
    store.upsert("alice", "https://github.com/o/r/pull/1", "def456")
    listed = store.list_for("alice")
    assert len(listed) == 1
    assert listed[0].last_reviewed_sha == "def456"


def test_upsert_with_null_sha(tmp_path):
    store = ReviewQueueStore(tmp_path / "rq.json")
    entry = store.upsert("alice", "https://github.com/o/r/pull/1", None)
    assert entry.last_reviewed_sha is None


def test_delete_removes_entry(tmp_path):
    store = ReviewQueueStore(tmp_path / "rq.json")
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    store.upsert("alice", "https://github.com/o/r/pull/2", "def")
    assert store.delete("alice", "https://github.com/o/r/pull/1") is True
    remaining = store.list_for("alice")
    assert len(remaining) == 1
    assert remaining[0].pr_url == "https://github.com/o/r/pull/2"


def test_delete_missing_returns_false(tmp_path):
    store = ReviewQueueStore(tmp_path / "rq.json")
    assert store.delete("alice", "https://github.com/o/r/pull/1") is False


def test_delete_last_entry_removes_reviewer_key(tmp_path):
    path = tmp_path / "rq.json"
    store = ReviewQueueStore(path)
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    assert store.delete("alice", "https://github.com/o/r/pull/1") is True
    raw = json.loads(path.read_text())
    assert "alice" not in raw


def test_persistence_round_trip(tmp_path):
    path = tmp_path / "rq.json"
    s1 = ReviewQueueStore(path)
    s1.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    s1.upsert("bob", "https://github.com/o/r/pull/2", None)

    s2 = ReviewQueueStore(path)
    alice = s2.list_for("alice")
    bob = s2.list_for("bob")
    assert len(alice) == 1 and alice[0].last_reviewed_sha == "abc"
    assert len(bob) == 1 and bob[0].last_reviewed_sha is None


def test_corrupt_file_does_not_crash(tmp_path):
    path = tmp_path / "rq.json"
    path.write_text("{not json")
    store = ReviewQueueStore(path)
    assert store.list_for("alice") == []
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    assert len(store.list_for("alice")) == 1


def test_atomic_write_no_tmp_leftover(tmp_path):
    path = tmp_path / "rq.json"
    store = ReviewQueueStore(path)
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    assert path.exists()
    assert not (path.parent / (path.name + ".tmp")).exists()
