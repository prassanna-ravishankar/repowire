"""Happy encryption implementation.

This module ports the encryption logic from Happy's TypeScript implementation
to Python using PyNaCl and the cryptography library.

Reference files:
- happy/sources/encryption/deriveKey.ts
- happy/sources/encryption/libsodium.ts
- happy/sources/sync/encryption/encryptor.ts
- happy/sources/sync/encryption/encryption.ts
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
from abc import ABC, abstractmethod
from typing import Any

from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from nacl.public import Box, PrivateKey, PublicKey
from nacl.secret import SecretBox
from nacl.utils import random as nacl_random


def derive_key(master: bytes, usage: str, path: list[str]) -> bytes:
    """Derive a key using HMAC-SHA512 key derivation.

    Matches Happy's deriveKey function from deriveKey.ts.
    """
    key_material = (usage + " Master Seed").encode()
    I = hmac.new(key_material, master, hashlib.sha512).digest()
    key = I[:32]
    chain_code = I[32:]

    for index in path:
        data = b"\x00" + index.encode()
        I = hmac.new(chain_code, data, hashlib.sha512).digest()
        key = I[:32]
        chain_code = I[32:]

    return key


class Encryptor(ABC):
    @abstractmethod
    def encrypt(self, data: list[Any]) -> list[bytes]:
        """Encrypt a list of data items."""
        pass


class Decryptor(ABC):
    @abstractmethod
    def decrypt(self, data: list[bytes]) -> list[Any | None]:
        """Decrypt a list of encrypted items."""
        pass


class SecretBoxEncryption(Encryptor, Decryptor):
    """NaCl SecretBox encryption (XSalsa20-Poly1305).

    Used for legacy encryption with the master secret.
    """

    NONCE_SIZE = 24

    def __init__(self, secret_key: bytes) -> None:
        self.box = SecretBox(secret_key)

    def encrypt(self, data: list[Any]) -> list[bytes]:
        results: list[bytes] = []
        for item in data:
            nonce = nacl_random(self.NONCE_SIZE)
            plaintext = json.dumps(item).encode()
            encrypted = self.box.encrypt(plaintext, nonce)
            # Format: nonce + ciphertext (encrypted already includes nonce prefix)
            results.append(bytes(encrypted))
        return results

    def decrypt(self, data: list[bytes]) -> list[Any | None]:
        results: list[Any | None] = []
        for item in data:
            try:
                decrypted = self.box.decrypt(item)
                results.append(json.loads(decrypted.decode()))
            except Exception:
                results.append(None)
        return results


class BoxEncryption(Encryptor, Decryptor):
    """NaCl Box encryption (Curve25519-XSalsa20-Poly1305).

    Used for public-key encryption with ephemeral keys.
    """

    PUBLIC_KEY_SIZE = 32
    NONCE_SIZE = 24

    def __init__(self, seed: bytes) -> None:
        self.private_key = PrivateKey(seed)
        self.public_key = self.private_key.public_key

    def encrypt(self, data: list[Any]) -> list[bytes]:
        results: list[bytes] = []
        for item in data:
            ephemeral_key = PrivateKey.generate()
            nonce = nacl_random(self.NONCE_SIZE)
            box = Box(ephemeral_key, self.public_key)
            plaintext = json.dumps(item).encode()
            encrypted = box.encrypt(plaintext, nonce)

            # Bundle: ephemeral public key (32) + nonce (24) + ciphertext
            result = bytes(ephemeral_key.public_key) + bytes(encrypted)
            results.append(result)
        return results

    def decrypt(self, data: list[bytes]) -> list[Any | None]:
        results: list[Any | None] = []
        for item in data:
            try:
                ephemeral_public = PublicKey(item[: self.PUBLIC_KEY_SIZE])
                encrypted = item[self.PUBLIC_KEY_SIZE :]
                box = Box(self.private_key, ephemeral_public)
                decrypted = box.decrypt(encrypted)
                results.append(json.loads(decrypted.decode()))
            except Exception:
                results.append(None)
        return results


class AES256Encryption(Encryptor, Decryptor):
    """AES-256-GCM encryption.

    Used for session-specific encryption with per-session keys.
    Format: version byte (0x00) + IV (12 bytes) + ciphertext + tag (16 bytes)
    """

    IV_SIZE = 12
    TAG_SIZE = 16

    def __init__(self, secret_key: bytes) -> None:
        self.aesgcm = AESGCM(secret_key)

    def encrypt(self, data: list[Any]) -> list[bytes]:
        results: list[bytes] = []
        for item in data:
            iv = os.urandom(self.IV_SIZE)
            plaintext = json.dumps(item).encode()
            ciphertext = self.aesgcm.encrypt(iv, plaintext, None)

            # Format: version (1) + iv (12) + ciphertext+tag
            output = bytes([0]) + iv + ciphertext
            results.append(output)
        return results

    def decrypt(self, data: list[bytes]) -> list[Any | None]:
        results: list[Any | None] = []
        for item in data:
            try:
                if item[0] != 0:
                    results.append(None)
                    continue
                iv = item[1 : 1 + self.IV_SIZE]
                ciphertext = item[1 + self.IV_SIZE :]
                plaintext = self.aesgcm.decrypt(iv, ciphertext, None)
                results.append(json.loads(plaintext.decode()))
            except Exception:
                results.append(None)
        return results


class HappyEncryption:
    """Main encryption class for Happy Cloud.

    Manages legacy encryption (SecretBox) and session-specific encryption (AES-256-GCM).
    """

    def __init__(self, master_secret: bytes) -> None:
        self.master_secret = master_secret
        self.legacy_encryption = SecretBoxEncryption(master_secret)

        # Derive content key for decrypting session encryption keys
        content_data_key = derive_key(master_secret, "Happy EnCoder", ["content"])
        self.content_private_key = PrivateKey(content_data_key)
        self.content_public_key = self.content_private_key.public_key

        # Session encryption cache
        self._session_encryptions: dict[str, SessionEncryption] = {}

    def decrypt_encryption_key(self, encrypted_b64: str) -> bytes | None:
        """Decrypt a session encryption key using the content private key.

        The encrypted key format is: version byte (0x00) + ephemeral_pubkey (32) + nonce (24) + ciphertext
        """
        try:
            encrypted = base64.b64decode(encrypted_b64)
            if encrypted[0] != 0:
                return None

            # Extract components: ephemeral pubkey (32) + nonce (24) + ciphertext
            encrypted_data = encrypted[1:]
            ephemeral_public = PublicKey(encrypted_data[:32])
            nonce = encrypted_data[32:56]  # 24 bytes
            ciphertext = encrypted_data[56:]

            # Decrypt using Box with explicit nonce
            box = Box(self.content_private_key, ephemeral_public)
            decrypted = box.decrypt(ciphertext, nonce)
            return bytes(decrypted)
        except Exception:
            return None

    def encrypt_encryption_key(self, key: bytes) -> bytes:
        """Encrypt a session encryption key using the content public key."""
        ephemeral_key = PrivateKey.generate()
        nonce = nacl_random(24)
        box = Box(ephemeral_key, self.content_public_key)
        encrypted = box.encrypt(key, nonce)

        # Format: version (0x00) + ephemeral public key + encrypted
        result = bytes([0]) + bytes(ephemeral_key.public_key) + bytes(encrypted)
        return result

    def initialize_session(
        self, session_id: str, data_key: bytes | None
    ) -> SessionEncryption:
        """Initialize encryption for a session."""
        if data_key is None:
            encryptor = self.legacy_encryption
        else:
            encryptor = AES256Encryption(data_key)

        session_enc = SessionEncryption(session_id, encryptor)
        self._session_encryptions[session_id] = session_enc
        return session_enc

    def get_session_encryption(self, session_id: str) -> SessionEncryption | None:
        """Get encryption for a session if initialized."""
        return self._session_encryptions.get(session_id)

    def encrypt_raw(self, data: Any) -> str:
        """Encrypt data using legacy encryption."""
        encrypted = self.legacy_encryption.encrypt([data])
        return base64.b64encode(encrypted[0]).decode()

    def decrypt_raw(self, encrypted: str) -> Any | None:
        """Decrypt data using legacy encryption."""
        try:
            encrypted_bytes = base64.b64decode(encrypted)
            decrypted = self.legacy_encryption.decrypt([encrypted_bytes])
            return decrypted[0]
        except Exception:
            return None


class SessionEncryption:
    """Session-specific encryption using AES-256-GCM or legacy SecretBox."""

    def __init__(
        self, session_id: str, encryptor: Encryptor & Decryptor  # type: ignore
    ) -> None:
        self.session_id = session_id
        self.encryptor = encryptor

    def encrypt_raw_record(self, record: dict[str, Any]) -> str:
        """Encrypt a RawRecord for sending as a message."""
        encrypted = self.encryptor.encrypt([record])
        return base64.b64encode(encrypted[0]).decode()

    def decrypt_raw(self, encrypted: str) -> Any | None:
        """Decrypt received data."""
        try:
            encrypted_bytes = base64.b64decode(encrypted)
            decrypted = self.encryptor.decrypt([encrypted_bytes])
            return decrypted[0]
        except Exception:
            return None

    def encrypt_raw(self, data: Any) -> str:
        """Encrypt arbitrary data."""
        encrypted = self.encryptor.encrypt([data])
        return base64.b64encode(encrypted[0]).decode()
