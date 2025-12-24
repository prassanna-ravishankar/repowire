import json
import pytest
from pathlib import Path

from repowire.auth.happy import (
    HappyCredentials,
    load_credentials,
    save_credentials,
    decode_secret,
    derive_key,
    CREDENTIALS_PATH,
)


class TestHappyCredentials:
    def test_credentials_dataclass(self):
        creds = HappyCredentials(token="test-token", secret="test-secret")
        assert creds.token == "test-token"
        assert creds.secret == "test-secret"


class TestCredentialsStorage:
    @pytest.fixture
    def temp_creds_file(self, tmp_path, monkeypatch):
        creds_path = tmp_path / "credentials.json"
        monkeypatch.setattr(
            "repowire.auth.happy.CREDENTIALS_PATH", creds_path
        )
        return creds_path

    def test_save_and_load_credentials(self, temp_creds_file):
        creds = HappyCredentials(token="test-token", secret="test-secret")
        save_credentials(creds)
        loaded = load_credentials()
        assert loaded is not None
        assert loaded.token == "test-token"
        assert loaded.secret == "test-secret"

    def test_load_nonexistent_credentials(self, temp_creds_file):
        result = load_credentials()
        assert result is None

    def test_save_creates_parent_directory(self, tmp_path, monkeypatch):
        creds_path = tmp_path / "nested" / "dir" / "credentials.json"
        monkeypatch.setattr("repowire.auth.happy.CREDENTIALS_PATH", creds_path)
        creds = HappyCredentials(token="test-token", secret="test-secret")
        save_credentials(creds)
        assert creds_path.exists()

    def test_save_preserves_other_keys(self, temp_creds_file):
        temp_creds_file.parent.mkdir(parents=True, exist_ok=True)
        temp_creds_file.write_text(json.dumps({"other_key": "other_value"}))

        creds = HappyCredentials(token="test-token", secret="test-secret")
        save_credentials(creds)

        content = json.loads(temp_creds_file.read_text())
        assert content["other_key"] == "other_value"
        assert content["happy"]["token"] == "test-token"


class TestDecodeSecret:
    def test_decode_base64url_secret(self):
        secret_b64 = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE"
        result = decode_secret(secret_b64)
        assert len(result) == 32
        assert result == b"01234567890123456789012345678901"

    def test_decode_with_padding(self):
        secret_b64 = "dGVzdA"
        result = decode_secret(secret_b64)
        assert result == b"test"


class TestDeriveKey:
    @pytest.mark.asyncio
    async def test_derive_key_basic(self):
        master = b"0" * 32
        key = await derive_key(master, "Test Usage", ["path1"])
        assert len(key) == 32
        assert isinstance(key, bytes)

    @pytest.mark.asyncio
    async def test_derive_key_deterministic(self):
        master = b"0" * 32
        key1 = await derive_key(master, "Test Usage", ["path1"])
        key2 = await derive_key(master, "Test Usage", ["path1"])
        assert key1 == key2

    @pytest.mark.asyncio
    async def test_derive_key_different_paths(self):
        master = b"0" * 32
        key1 = await derive_key(master, "Test Usage", ["path1"])
        key2 = await derive_key(master, "Test Usage", ["path2"])
        assert key1 != key2

    @pytest.mark.asyncio
    async def test_derive_key_different_usage(self):
        master = b"0" * 32
        key1 = await derive_key(master, "Usage A", ["path"])
        key2 = await derive_key(master, "Usage B", ["path"])
        assert key1 != key2

    @pytest.mark.asyncio
    async def test_derive_key_nested_path(self):
        master = b"0" * 32
        key = await derive_key(master, "Test Usage", ["path1", "path2", "path3"])
        assert len(key) == 32
