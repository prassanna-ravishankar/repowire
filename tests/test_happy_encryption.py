import base64
import json
import os

import pytest

from repowire.transport.happy_encryption import (
    AES256Encryption,
    BoxEncryption,
    HappyEncryption,
    SecretBoxEncryption,
    SessionEncryption,
    derive_key,
)


class TestDeriveKey:
    def test_derive_key_basic(self):
        master = b"0" * 32
        key = derive_key(master, "Test Usage", ["path1"])
        assert len(key) == 32
        assert isinstance(key, bytes)

    def test_derive_key_deterministic(self):
        master = b"0" * 32
        key1 = derive_key(master, "Test Usage", ["path1"])
        key2 = derive_key(master, "Test Usage", ["path1"])
        assert key1 == key2

    def test_derive_key_different_paths(self):
        master = b"0" * 32
        key1 = derive_key(master, "Test Usage", ["path1"])
        key2 = derive_key(master, "Test Usage", ["path2"])
        assert key1 != key2

    def test_derive_key_different_usage(self):
        master = b"0" * 32
        key1 = derive_key(master, "Usage A", ["path"])
        key2 = derive_key(master, "Usage B", ["path"])
        assert key1 != key2

    def test_derive_key_nested_path(self):
        master = b"0" * 32
        key = derive_key(master, "Test Usage", ["path1", "path2", "path3"])
        assert len(key) == 32


class TestSecretBoxEncryption:
    @pytest.fixture
    def secret_key(self):
        return os.urandom(32)

    @pytest.fixture
    def encryptor(self, secret_key):
        return SecretBoxEncryption(secret_key)

    def test_encrypt_decrypt_string(self, encryptor):
        data = ["hello world"]
        encrypted = encryptor.encrypt(data)
        assert len(encrypted) == 1
        assert encrypted[0] != data[0].encode()

        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_dict(self, encryptor):
        data = [{"key": "value", "number": 42}]
        encrypted = encryptor.encrypt(data)
        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_list(self, encryptor):
        data = [[1, 2, 3], {"nested": True}]
        encrypted = encryptor.encrypt(data)
        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_decrypt_invalid_data(self, encryptor):
        invalid_data = [b"not encrypted"]
        decrypted = encryptor.decrypt(invalid_data)
        assert decrypted == [None]


class TestBoxEncryption:
    @pytest.fixture
    def seed(self):
        return os.urandom(32)

    @pytest.fixture
    def encryptor(self, seed):
        return BoxEncryption(seed)

    def test_encrypt_decrypt_string(self, encryptor):
        data = ["hello world"]
        encrypted = encryptor.encrypt(data)
        assert len(encrypted) == 1

        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_dict(self, encryptor):
        data = [{"key": "value", "number": 42}]
        encrypted = encryptor.encrypt(data)
        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_decrypt_invalid_data(self, encryptor):
        invalid_data = [b"not encrypted at all"]
        decrypted = encryptor.decrypt(invalid_data)
        assert decrypted == [None]


class TestAES256Encryption:
    @pytest.fixture
    def secret_key(self):
        return os.urandom(32)

    @pytest.fixture
    def encryptor(self, secret_key):
        return AES256Encryption(secret_key)

    def test_encrypt_decrypt_string(self, encryptor):
        data = ["hello world"]
        encrypted = encryptor.encrypt(data)
        assert len(encrypted) == 1
        assert encrypted[0][0] == 0  # Version byte

        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_dict(self, encryptor):
        data = [{"key": "value", "number": 42}]
        encrypted = encryptor.encrypt(data)
        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_complex_data(self, encryptor):
        data = [
            {
                "role": "user",
                "content": {"type": "text", "text": "Hello, world!"},
                "meta": {"sentFrom": "repowire"},
            }
        ]
        encrypted = encryptor.encrypt(data)
        decrypted = encryptor.decrypt(encrypted)
        assert decrypted == data

    def test_decrypt_wrong_version(self, encryptor):
        # Data with wrong version byte
        invalid_data = [bytes([1]) + os.urandom(32)]
        decrypted = encryptor.decrypt(invalid_data)
        assert decrypted == [None]


class TestHappyEncryption:
    @pytest.fixture
    def master_secret(self):
        return os.urandom(32)

    @pytest.fixture
    def encryption(self, master_secret):
        return HappyEncryption(master_secret)

    def test_legacy_encrypt_decrypt(self, encryption):
        data = {"key": "value"}
        encrypted = encryption.encrypt_raw(data)
        decrypted = encryption.decrypt_raw(encrypted)
        assert decrypted == data

    def test_encrypt_decrypt_encryption_key(self, encryption):
        # Generate a session key
        session_key = os.urandom(32)

        # Encrypt it
        encrypted = encryption.encrypt_encryption_key(session_key)

        # Decrypt it
        decrypted = encryption.decrypt_encryption_key(base64.b64encode(encrypted).decode())

        assert decrypted == session_key

    def test_initialize_session_with_key(self, encryption):
        session_id = "test-session-123"
        data_key = os.urandom(32)

        session_enc = encryption.initialize_session(session_id, data_key)

        assert session_enc is not None
        assert encryption.get_session_encryption(session_id) is session_enc

    def test_initialize_session_without_key(self, encryption):
        session_id = "test-session-legacy"

        session_enc = encryption.initialize_session(session_id, None)

        assert session_enc is not None
        # Should use legacy encryption

    def test_get_nonexistent_session(self, encryption):
        assert encryption.get_session_encryption("nonexistent") is None


class TestSessionEncryption:
    @pytest.fixture
    def session_encryption(self):
        secret_key = os.urandom(32)
        encryptor = AES256Encryption(secret_key)
        return SessionEncryption("test-session", encryptor)

    def test_encrypt_raw_record(self, session_encryption):
        record = {
            "role": "user",
            "content": {"type": "text", "text": "Hello!"},
            "meta": {"sentFrom": "repowire"},
        }
        encrypted = session_encryption.encrypt_raw_record(record)

        # Should be base64 encoded
        assert isinstance(encrypted, str)

        # Should be able to decrypt
        decrypted = session_encryption.decrypt_raw(encrypted)
        assert decrypted == record

    def test_encrypt_decrypt_raw(self, session_encryption):
        data = {"arbitrary": "data", "number": 42}
        encrypted = session_encryption.encrypt_raw(data)
        decrypted = session_encryption.decrypt_raw(encrypted)
        assert decrypted == data
