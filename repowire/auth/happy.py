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
DEFAULT_HAPPY_URL = "https://api.happycoder.io"


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
