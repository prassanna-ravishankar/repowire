"""Tests for the relay share token registry."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from repowire.relay.share_tokens import (
    ShareToken,
    _registry,
    create_share_token,
    list_share_tokens,
    revoke_share_token,
    validate_share_token,
)


@pytest.fixture(autouse=True)
def _clear_registry():
    _registry.clear()
    yield
    _registry.clear()


class TestCreateShareToken:
    def test_creates_token_with_correct_fields(self):
        t = create_share_token("user1", "my-agent", "ro")
        assert t.share_id.startswith("sh_")
        assert t.user_id == "user1"
        assert t.peer_name == "my-agent"
        assert t.permissions == "ro"
        assert t.expires_at is None

    def test_rw_permissions(self):
        t = create_share_token("user1", "my-agent", "rw")
        assert t.permissions == "rw"

    def test_ttl_sets_expires_at(self):
        before = datetime.now(timezone.utc)
        t = create_share_token("user1", "my-agent", "ro", ttl_secs=3600)
        after = datetime.now(timezone.utc)
        assert t.expires_at is not None
        assert before + timedelta(seconds=3590) < t.expires_at < after + timedelta(seconds=3610)

    def test_unique_share_ids(self):
        ids = {create_share_token("u", "p", "ro").share_id for _ in range(20)}
        assert len(ids) == 20


class TestValidateShareToken:
    def test_valid_token_returned(self):
        t = create_share_token("user1", "my-agent", "ro")
        assert validate_share_token(t.share_id) is t

    def test_unknown_token_returns_none(self):
        assert validate_share_token("sh_notexist") is None

    def test_expired_token_returns_none_and_is_removed(self):
        t = create_share_token("user1", "my-agent", "ro", ttl_secs=1)
        # Manually backdate
        t.expires_at = datetime.now(timezone.utc) - timedelta(seconds=1)
        result = validate_share_token(t.share_id)
        assert result is None
        assert t.share_id not in _registry

    def test_non_expired_token_still_valid(self):
        t = create_share_token("user1", "my-agent", "ro", ttl_secs=9999)
        assert validate_share_token(t.share_id) is t


class TestRevokeShareToken:
    def test_revoke_removes_token(self):
        t = create_share_token("user1", "my-agent", "ro")
        assert revoke_share_token(t.share_id) is True
        assert validate_share_token(t.share_id) is None

    def test_revoke_nonexistent_returns_false(self):
        assert revoke_share_token("sh_ghost") is False


class TestListShareTokens:
    def test_returns_only_user_tokens(self):
        a = create_share_token("alice", "p1", "ro")
        create_share_token("bob", "p2", "ro")
        tokens = list_share_tokens("alice")
        assert len(tokens) == 1
        assert tokens[0] is a

    def test_excludes_expired(self):
        t = create_share_token("alice", "p1", "ro", ttl_secs=1)
        t.expires_at = datetime.now(timezone.utc) - timedelta(seconds=1)
        assert list_share_tokens("alice") == []


class TestShareTokenToDict:
    def test_to_dict_fields(self):
        t = create_share_token("user1", "my-agent", "rw")
        d = t.to_dict()
        assert d["share_id"] == t.share_id
        assert d["peer_name"] == "my-agent"
        assert d["permissions"] == "rw"
        assert d["expires_at"] is None
        assert "created_at" in d
