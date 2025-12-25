from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
from dataclasses import dataclass
from pathlib import Path

import httpx
from nacl.signing import SigningKey

CREDENTIALS_PATH = Path.home() / ".repowire" / "credentials.json"
DEFAULT_HAPPY_URL = "https://api.cluster-fluster.com"


@dataclass
class HappyCredentials:
    token: str
    secret: str  # Base64URL encoded 32-byte secret


def load_credentials() -> HappyCredentials | None:
    if not CREDENTIALS_PATH.exists():
        return None
    data = json.loads(CREDENTIALS_PATH.read_text())
    if "happy" in data:
        return HappyCredentials(**data["happy"])
    return None


def save_credentials(creds: HappyCredentials) -> None:
    CREDENTIALS_PATH.parent.mkdir(parents=True, exist_ok=True)
    existing: dict = {}
    if CREDENTIALS_PATH.exists():
        existing = json.loads(CREDENTIALS_PATH.read_text())
    existing["happy"] = {"token": creds.token, "secret": creds.secret}
    CREDENTIALS_PATH.write_text(json.dumps(existing, indent=2))


def decode_secret(secret_b64: str) -> bytes:
    """Decode base64url-encoded secret."""
    padding = 4 - len(secret_b64) % 4
    if padding != 4:
        secret_b64 += "=" * padding
    return base64.urlsafe_b64decode(secret_b64)


# Base32 alphabet (RFC 4648) - same as Happy uses
BASE32_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"


def parse_backup_secret_key(formatted_key: str) -> str:
    """Parse a user-friendly formatted secret key back to base64url.

    Accepts format like: XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX
    Returns base64url encoded secret key.
    """
    # Normalize: uppercase, replace common mistakes, remove non-base32 chars
    normalized = formatted_key.upper()
    normalized = normalized.replace("0", "O").replace("1", "I").replace("8", "B").replace("9", "G")
    cleaned = "".join(c for c in normalized if c in BASE32_ALPHABET)

    if not cleaned:
        raise ValueError("No valid characters found")

    # Decode base32 to bytes
    bytes_list: list[int] = []
    buffer = 0
    buffer_length = 0

    for char in cleaned:
        value = BASE32_ALPHABET.index(char)
        buffer = (buffer << 5) | value
        buffer_length += 5

        if buffer_length >= 8:
            buffer_length -= 8
            bytes_list.append((buffer >> buffer_length) & 0xFF)

    secret_bytes = bytes(bytes_list)

    if len(secret_bytes) != 32:
        raise ValueError(f"Invalid key length: expected 32 bytes, got {len(secret_bytes)}")

    # Encode to base64url (no padding)
    return base64.urlsafe_b64encode(secret_bytes).rstrip(b"=").decode()


def normalize_secret_key(key: str) -> str:
    """Normalize a secret key to base64url format.

    Accepts either:
    - Base64url encoded secret (44 chars)
    - Backup format: XXXXX-XXXXX-XXXXX-... (base32 with dashes)
    """
    trimmed = key.strip()

    # If it has dashes/spaces or is long, treat as backup format
    if "-" in trimmed or " " in trimmed or len(trimmed) > 50:
        return parse_backup_secret_key(trimmed)

    # Otherwise try as base64url
    try:
        secret_bytes = decode_secret(trimmed)
        if len(secret_bytes) != 32:
            raise ValueError("Invalid secret key")
        return trimmed
    except Exception:
        # Fall back to parsing as backup format
        return parse_backup_secret_key(trimmed)


async def get_token(secret: bytes, server_url: str = DEFAULT_HAPPY_URL) -> str:
    """Get auth token using challenge/signature mechanism."""
    signing_key = SigningKey(secret)
    challenge = os.urandom(32)
    signed = signing_key.sign(challenge)

    async with httpx.AsyncClient() as client:
        response = await client.post(
            f"{server_url}/v1/auth",
            json={
                "challenge": base64.b64encode(challenge).decode(),
                "signature": base64.b64encode(signed.signature).decode(),
                "publicKey": base64.b64encode(
                    signing_key.verify_key.encode()
                ).decode(),
            },
        )
        response.raise_for_status()
        return response.json()["token"]


async def derive_key(master: bytes, usage: str, path: list[str]) -> bytes:
    """Derive a key using HMAC-SHA512 key derivation (matches Happy's deriveKey)."""
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
