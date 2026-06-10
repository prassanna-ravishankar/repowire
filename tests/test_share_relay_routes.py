"""Tests for relay share session routes."""

from __future__ import annotations

import pytest
from httpx import ASGITransport, AsyncClient

pytestmark = pytest.mark.anyio

from repowire.relay.auth import _token_registry, register_token
from repowire.relay.server import _connections, _user_daemons, create_app
from repowire.relay.share_tokens import _registry as _share_registry


@pytest.fixture(autouse=True)
def _clean_state():
    _token_registry.clear()
    _connections.clear()
    _user_daemons.clear()
    _share_registry.clear()
    yield
    _token_registry.clear()
    _connections.clear()
    _user_daemons.clear()
    _share_registry.clear()


@pytest.fixture
def app():
    return create_app()


@pytest.fixture
async def client(app):
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as c:
        yield c


@pytest.fixture
def api_key():
    return register_token("testuser").key


class TestShareCreate:
    async def test_requires_auth(self, client):
        resp = await client.post("/api/v1/share", json={"peer_name": "agent1"})
        assert resp.status_code == 422  # missing x-api-key header

    async def test_no_daemon_returns_502(self, client, api_key):
        resp = await client.post(
            "/api/v1/share",
            json={"peer_name": "agent1"},
            headers={"x-api-key": api_key},
        )
        assert resp.status_code == 502

    async def test_invalid_permissions_returns_400(self, client, api_key, monkeypatch):
        # Patch to fake a connected daemon
        from repowire.relay import server as srv
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        resp = await client.post(
            "/api/v1/share",
            json={"peer_name": "agent1", "permissions": "admin"},
            headers={"x-api-key": api_key},
        )
        assert resp.status_code == 400

    async def test_creates_token_when_daemon_connected(self, client, api_key, monkeypatch):
        from repowire.relay import server as srv
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        resp = await client.post(
            "/api/v1/share",
            json={"peer_name": "agent1", "permissions": "ro"},
            headers={"x-api-key": api_key},
        )
        assert resp.status_code == 200
        data = resp.json()
        assert data["share_id"].startswith("sh_")
        assert data["peer_name"] == "agent1"
        assert data["permissions"] == "ro"


class TestShareList:
    async def test_empty_list(self, client, api_key, monkeypatch):
        resp = await client.get("/api/v1/share", headers={"x-api-key": api_key})
        assert resp.status_code == 200
        assert resp.json() == []

    async def test_lists_own_tokens(self, client, api_key, monkeypatch):
        from repowire.relay import server as srv
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        await client.post(
            "/api/v1/share",
            json={"peer_name": "p1", "permissions": "ro"},
            headers={"x-api-key": api_key},
        )
        resp = await client.get("/api/v1/share", headers={"x-api-key": api_key})
        assert resp.status_code == 200
        tokens = resp.json()
        assert len(tokens) == 1
        assert tokens[0]["peer_name"] == "p1"


class TestShareRevoke:
    async def test_revoke_nonexistent_returns_404(self, client, api_key):
        resp = await client.delete("/api/v1/share/sh_ghost", headers={"x-api-key": api_key})
        assert resp.status_code == 404

    async def test_revoke_own_token(self, client, api_key, monkeypatch):
        from repowire.relay import server as srv
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        create_resp = await client.post(
            "/api/v1/share",
            json={"peer_name": "p1"},
            headers={"x-api-key": api_key},
        )
        share_id = create_resp.json()["share_id"]
        revoke_resp = await client.delete(f"/api/v1/share/{share_id}", headers={"x-api-key": api_key})
        assert revoke_resp.status_code == 200
        assert revoke_resp.json()["ok"] is True

    async def test_cannot_revoke_others_token(self, client, monkeypatch):
        from repowire.relay import server as srv
        from repowire.relay.auth import register_token as rt
        from repowire.relay.share_tokens import create_share_token
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "alice"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        token = create_share_token("alice", "p1", "ro")
        bob_key = rt("bob").key
        resp = await client.delete(f"/api/v1/share/{token.share_id}", headers={"x-api-key": bob_key})
        assert resp.status_code == 403


class TestShareViewer:
    async def test_unknown_share_returns_404(self, client):
        resp = await client.get("/s/sh_unknown")
        assert resp.status_code == 404

    async def test_no_daemon_returns_503(self, client):
        from repowire.relay.share_tokens import create_share_token
        token = create_share_token("testuser", "p1", "ro")
        resp = await client.get(f"/s/{token.share_id}")
        assert resp.status_code == 503

    async def test_ro_viewer_contains_peer_name(self, client, monkeypatch):
        from repowire.relay import server as srv
        from repowire.relay.share_tokens import create_share_token
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        token = create_share_token("testuser", "my-agent", "ro")
        resp = await client.get(f"/s/{token.share_id}")
        assert resp.status_code == 200
        assert "my-agent" in resp.text
        assert "read-only" in resp.text
        assert "send-btn" not in resp.text

    async def test_rw_viewer_contains_compose(self, client, monkeypatch):
        from repowire.relay import server as srv
        from repowire.relay.share_tokens import create_share_token
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        token = create_share_token("testuser", "my-agent", "rw")
        resp = await client.get(f"/s/{token.share_id}")
        assert resp.status_code == 200
        assert "read-write" in resp.text
        assert "send-btn" in resp.text


class TestShareAsk:
    async def test_ro_ask_rejected(self, client, monkeypatch):
        from repowire.relay import server as srv
        from repowire.relay.share_tokens import create_share_token
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        token = create_share_token("testuser", "p1", "ro")
        resp = await client.post(f"/s/{token.share_id}/ask", json={"text": "hello"})
        assert resp.status_code == 403

    async def test_empty_text_rejected(self, client, monkeypatch):
        from repowire.relay import server as srv
        from repowire.relay.share_tokens import create_share_token
        from unittest.mock import MagicMock
        fake_conn = MagicMock()
        fake_conn.user_id = "testuser"
        monkeypatch.setattr(srv, "_get_any_daemon", lambda uid: fake_conn)
        token = create_share_token("testuser", "p1", "rw")
        resp = await client.post(f"/s/{token.share_id}/ask", json={"text": "  "})
        assert resp.status_code == 400
