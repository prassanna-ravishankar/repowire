"""Ingress peer: verification, idempotency, routing, and status codes."""

from __future__ import annotations

import hashlib
import hmac
import time
from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient

from repowire.client import DaemonConnectionError
from repowire.config.models import (
    FederationConfig,
    IngressConfig,
    IngressSource,
    IngressTarget,
    VerifyConfig,
)
from repowire.ingress import bot

SECRET = "topsecret"
BODY = b'{"number": 42, "action": "opened"}'


def _sign(secret: str, body: bytes) -> str:
    return "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()


@pytest.fixture
def client(monkeypatch: pytest.MonkeyPatch) -> TestClient:
    monkeypatch.setenv("GH_SECRET", SECRET)
    source = IngressSource(
        verify=VerifyConfig(
            scheme="hmac",
            header="X-Hub-Signature-256",
            prefix="sha256=",
            secret_ref="GH_SECRET",
            delivery_id_header="X-GitHub-Delivery",
        ),
        target=IngressTarget(kind="ask", to_peer="reviewer"),
        template="PR #{number} {action}",
    )
    peer = bot.IngressPeer(
        IngressConfig(enabled=True, sources={"gh-pr": source}), FederationConfig()
    )
    peer._client.ask = AsyncMock(  # type: ignore[method-assign]
        return_value=type("R", (), {"correlation_id": "ask-xyz"})()
    )
    peer._client.notify = AsyncMock()  # type: ignore[method-assign]
    peer._client.create_job = AsyncMock()  # type: ignore[method-assign]
    tc = TestClient(peer._build_app())
    tc.peer = peer  # type: ignore[attr-defined]
    return tc


def _headers(body: bytes = BODY, delivery: str = "d1") -> dict[str, str]:
    return {
        "X-Hub-Signature-256": _sign(SECRET, body),
        "X-GitHub-Delivery": delivery,
        "Content-Type": "application/json",
    }


def test_signed_request_emits_templated_ask(client: TestClient) -> None:
    r = client.post("/ingress/gh-pr", content=BODY, headers=_headers())
    assert r.status_code == 200
    assert r.json() == {"status": "accepted", "correlation_id": "ask-xyz"}
    # Template rendered from the payload; from_peer is the namespaced principal.
    client.peer._client.ask.assert_awaited_once_with(  # type: ignore[attr-defined]
        "reviewer", "PR #42 opened", from_peer="ext-gh-pr", circle=None
    )


def test_replayed_delivery_is_deduped(client: TestClient) -> None:
    client.post("/ingress/gh-pr", content=BODY, headers=_headers())
    r = client.post("/ingress/gh-pr", content=BODY, headers=_headers())
    assert r.status_code == 200
    assert r.json()["status"] == "duplicate"
    # The duplicate must not emit a second ask.
    assert client.peer._client.ask.await_count == 1  # type: ignore[attr-defined]


def test_bad_signature_is_rejected_401(client: TestClient) -> None:
    bad = dict(_headers(), **{"X-Hub-Signature-256": "sha256=deadbeef"})
    r = client.post("/ingress/gh-pr", content=BODY, headers=bad)
    assert r.status_code == 401
    assert r.json()["reason"] == "signature_mismatch"
    client.peer._client.ask.assert_not_awaited()  # type: ignore[attr-defined]


def test_unknown_source_is_404(client: TestClient) -> None:
    r = client.post("/ingress/nope", content=BODY, headers=_headers())
    assert r.status_code == 404
    assert r.json()["reason"] == "unknown_source"


def test_missing_signature_is_401(client: TestClient) -> None:
    r = client.post("/ingress/gh-pr", content=BODY, headers={"Content-Type": "application/json"})
    assert r.status_code == 401


def test_failed_emit_is_503_and_replay_is_reattempted(client: TestClient) -> None:
    # A transient daemon outage must surface as 503 and NOT consume the
    # idempotency key — the provider's retry has to be re-emitted, not dropped.
    client.peer._client.ask = AsyncMock(  # type: ignore[attr-defined]
        side_effect=DaemonConnectionError()
    )
    r1 = client.post("/ingress/gh-pr", content=BODY, headers=_headers())
    assert r1.status_code == 503
    assert r1.json()["reason"] == "daemon_unavailable"

    # daemon recovers; the same delivery id must be re-attempted, not deduped.
    client.peer._client.ask = AsyncMock(  # type: ignore[attr-defined]
        return_value=type("R", (), {"correlation_id": "ask-2"})()
    )
    r2 = client.post("/ingress/gh-pr", content=BODY, headers=_headers())
    assert r2.status_code == 200
    assert r2.json()["correlation_id"] == "ask-2"


def test_missing_secret_is_500_not_401(monkeypatch: pytest.MonkeyPatch) -> None:
    # secret_ref points at an unset env var → operator misconfiguration → 500.
    monkeypatch.delenv("NOPE_SECRET", raising=False)
    source = IngressSource(
        verify=VerifyConfig(scheme="hmac", header="X-Sig", secret_ref="NOPE_SECRET"),
        target=IngressTarget(kind="ask", to_peer="x"),
    )
    peer = bot.IngressPeer(
        IngressConfig(enabled=True, sources={"s": source}), FederationConfig()
    )
    tc = TestClient(peer._build_app())
    r = tc.post("/ingress/s", content=BODY, headers={"X-Sig": "abc"})
    assert r.status_code == 500
    assert r.json()["reason"] == "missing_secret"


def test_oversized_body_is_413(client: TestClient) -> None:
    big = b"x" * (bot._BODY_MAX + 1)
    r = client.post("/ingress/gh-pr", content=big, headers={"X-Hub-Signature-256": "sha256=x"})
    assert r.status_code == 413


def test_rate_limit_returns_429(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RL_SECRET", SECRET)
    source = IngressSource(
        verify=VerifyConfig(scheme="hmac", header="X-Hub-Signature-256", prefix="sha256=",
                            secret_ref="RL_SECRET", delivery_id_header="X-GitHub-Delivery"),
        target=IngressTarget(kind="notify", to_peer="x"),
        rate_limit_per_min=1,
    )
    peer = bot.IngressPeer(
        IngressConfig(enabled=True, sources={"s": source}), FederationConfig()
    )
    peer._client.notify = AsyncMock()  # type: ignore[method-assign]
    tc = TestClient(peer._build_app())

    def post(delivery: str) -> int:
        h = {"X-Hub-Signature-256": _sign(SECRET, BODY), "X-GitHub-Delivery": delivery}
        return tc.post("/ingress/s", content=BODY, headers=h).status_code

    assert post("a") == 200
    assert post("b") == 429  # second within the same window exhausts the bucket


def test_trust_grant_emits_with_namespaced_principal(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("FED_SECRET", "grantsecret")
    from repowire.config.models import GrantConfig

    grant = GrantConfig(
        grant_id="grant_abc", issuer_mesh_id="alice", direction="inbound",
        allowed_kinds=["ask"], shared_secret_ref="FED_SECRET",
    )
    source = IngressSource(
        verify=VerifyConfig(scheme="trust_grant"),
        target=IngressTarget(kind="ask", to_peer="reviewer"),
        template="{text}",
    )
    fed = FederationConfig(mesh_id="bob", inbound_grants=[grant])
    peer = bot.IngressPeer(IngressConfig(enabled=True, sources={"f": source}), fed)
    peer._client.ask = AsyncMock(  # type: ignore[method-assign]
        return_value=type("R", (), {"correlation_id": "ask-f"})()
    )
    tc = TestClient(peer._build_app())

    body = b'{"text": "please review"}'
    sig = hmac.new(b"grantsecret", body, hashlib.sha256).hexdigest()
    r = tc.post(
        "/ingress/f",
        content=body,
        headers={"X-Repowire-Grant": "grant_abc", "X-Repowire-Grant-Sig": sig},
    )
    assert r.status_code == 200
    # from_peer is namespaced fed-* and carries the full grant id (no collision).
    peer._client.ask.assert_awaited_once_with(
        "reviewer", "please review", from_peer="fed-alice-grant_abc", circle=None
    )


def test_expired_grant_is_rejected(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("FED_SECRET", "grantsecret")
    from repowire.config.models import GrantConfig

    grant = GrantConfig(
        grant_id="grant_old", issuer_mesh_id="alice", direction="inbound",
        shared_secret_ref="FED_SECRET", expires_at="2000-01-01T00:00:00+00:00",
    )
    source = IngressSource(verify=VerifyConfig(scheme="trust_grant"),
                           target=IngressTarget(kind="ask", to_peer="r"))
    peer = bot.IngressPeer(
        IngressConfig(enabled=True, sources={"f": source}),
        FederationConfig(inbound_grants=[grant]),
    )
    tc = TestClient(peer._build_app())
    body = b"{}"
    sig = hmac.new(b"grantsecret", body, hashlib.sha256).hexdigest()
    r = tc.post("/ingress/f", content=body,
                headers={"X-Repowire-Grant": "grant_old", "X-Repowire-Grant-Sig": sig})
    assert r.status_code == 403  # out_of_scope (expired)


def _peer_with(source: IngressSource) -> TestClient:
    peer = bot.IngressPeer(
        IngressConfig(enabled=True, sources={"s": source}), FederationConfig()
    )
    peer._client.notify = AsyncMock()  # type: ignore[method-assign]
    tc = TestClient(peer._build_app())
    tc.peer = peer  # type: ignore[attr-defined]
    return tc


def test_stripe_style_timestamped_hmac(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("STRIPE_SECRET", SECRET)
    src = IngressSource(
        verify=VerifyConfig(
            scheme="hmac", header="Stripe-Signature", prefix="", encoding="hex",
            secret_ref="STRIPE_SECRET", payload_template="{ts}.{body}",
            sig_kv=True, ts_field="t", sig_field="v1", max_age_s=300,
        ),
        target=IngressTarget(kind="notify", to_peer="x"),
    )
    tc = _peer_with(src)
    body = b'{"id": "evt_1"}'

    def header(ts: str) -> dict[str, str]:
        signed = f"{ts}.{body.decode()}".encode()
        sig = hmac.new(SECRET.encode(), signed, hashlib.sha256).hexdigest()
        return {"Stripe-Signature": f"t={ts},v1={sig}"}

    fresh = tc.post("/ingress/s", content=body, headers=header(str(int(time.time()))))
    assert fresh.status_code == 200
    # A timestamp outside the replay window is rejected even with a valid HMAC.
    stale = tc.post("/ingress/s", content=body, headers=header(str(int(time.time()) - 10_000)))
    assert stale.status_code == 401


def test_slack_style_timestamped_hmac(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SLACK_SECRET", SECRET)
    src = IngressSource(
        verify=VerifyConfig(
            scheme="hmac", header="X-Slack-Signature", prefix="v0=", encoding="hex",
            secret_ref="SLACK_SECRET", payload_template="v0:{ts}:{body}",
            timestamp_header="X-Slack-Request-Timestamp",
        ),
        target=IngressTarget(kind="notify", to_peer="x"),
    )
    tc = _peer_with(src)
    body = b"token=abc&team_id=T1"
    ts = str(int(time.time()))
    sig = hmac.new(SECRET.encode(), f"v0:{ts}:{body.decode()}".encode(), hashlib.sha256).hexdigest()
    r = tc.post(
        "/ingress/s",
        content=body,
        headers={"X-Slack-Signature": f"v0={sig}", "X-Slack-Request-Timestamp": ts},
    )
    assert r.status_code == 200


def test_ed25519_signature(monkeypatch: pytest.MonkeyPatch) -> None:
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

    priv = Ed25519PrivateKey.generate()
    pub_pem = priv.public_key().public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
    ).decode()
    monkeypatch.setenv("DISCORD_PUBKEY", pub_pem)
    src = IngressSource(
        verify=VerifyConfig(
            scheme="ed25519", header="X-Signature-Ed25519", prefix="", encoding="hex",
            public_key_ref="DISCORD_PUBKEY",
        ),
        target=IngressTarget(kind="notify", to_peer="x"),
    )
    tc = _peer_with(src)
    body = b'{"type": 1}'
    sig = priv.sign(body).hex()
    ok = tc.post("/ingress/s", content=body, headers={"X-Signature-Ed25519": sig})
    assert ok.status_code == 200
    # tampered body fails verification
    bad = tc.post("/ingress/s", content=b'{"type": 2}', headers={"X-Signature-Ed25519": sig})
    assert bad.status_code == 401
