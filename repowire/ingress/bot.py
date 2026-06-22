"""Ingress peer for the repowire mesh.

Bridges the outside world <> repowire: a verified inbound webhook (P1) or a
trusted cross-mesh request (P2) becomes a standard mesh ask/notify/job. The peer
owns only the security envelope — verify, then emit through the daemon — so it
composes with jobs, schedule, the orchestrator, and the human surfaces without
adding a new message type. Routing is static: one registered source emits to one
fixed peer; the payload fills the text, it never selects the peer.

Unlike the other surfaces this peer is *inbound*: it hosts a localhost uvicorn
listener (reached publicly only through the relay tunnel) alongside the daemon
WebSocket connection it registers on.

Usage:
    repowire ingress start
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import logging
import os
import re
import time
from collections import deque
from collections.abc import Mapping
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from urllib.parse import urlparse, urlunparse

import uvicorn
import websockets
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from websockets.asyncio.client import ClientConnection

from repowire.client import (
    AsyncRepowireClient,
    DaemonConnectionError,
    DaemonHTTPError,
    DaemonTimeoutError,
)
from repowire.config.models import (
    DEFAULT_DAEMON_URL,
    FederationConfig,
    IngressConfig,
    IngressSource,
    VerifyConfig,
    load_config,
)
from repowire.protocol.peers import INGRESS_EXT_PREFIX, INGRESS_FED_PREFIX

logger = logging.getLogger(__name__)

# Capability floor for every ingress/federated principal. spawn/kill/schedule
# are never wired in this module — denial is structural; this set is the
# defense-in-depth check at the single emit chokepoint.
INGRESS_FLOOR = frozenset({"ask", "notify", "job"})

# Verifier reasons that mean the operator misconfigured the source (not a caller
# fault). Surfaced as 500, so a provider does not treat a setup error as a
# permanent auth failure.
_CONFIG_ERROR_REASONS = frozenset({"missing_secret", "scheme_not_enabled", "unknown_scheme"})

_DEDUP_MAX = 4096      # bounded in-process replay window (lost on restart, ok for v1)
_BODY_MAX = 1_048_576  # 1 MiB inbound body cap


# -- Pure helpers --


def _ws_url(http_url: str) -> str:
    """Convert an http(s) URL to ws(s)."""
    p = urlparse(http_url)
    return urlunparse(p._replace(scheme="wss" if p.scheme == "https" else "ws"))


def _resolve_ref(ref: str | None) -> str | None:
    """Resolve a secret/key reference to its value from the environment.

    Config stores only the *name* of the secret (optionally ``env:NAME``), never
    the secret itself — keeping public-repo configs free of credentials.
    """
    if not ref:
        return None
    if ref.startswith("env:"):
        ref = ref[4:]
    return os.environ.get(ref)


def _decode_sig(raw: str, encoding: str) -> bytes | None:
    """Decode a signature header value as hex or base64; None if malformed."""
    try:
        # binascii.Error (b64decode) subclasses ValueError, so this covers both.
        return bytes.fromhex(raw) if encoding == "hex" else base64.b64decode(raw)
    except ValueError:
        return None


def _is_expired(expires_at: str | None) -> bool:
    """True if an ISO-8601 grant expiry is in the past. Fails closed on garbage.

    Parsed to an aware datetime rather than string-compared, so offset forms
    (``+00:00``) and fractional seconds cannot make an expired grant read valid.
    """
    if not expires_at:
        return False
    try:
        exp = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
    except ValueError:
        return True
    if exp.tzinfo is None:
        exp = exp.replace(tzinfo=timezone.utc)
    return exp <= datetime.now(timezone.utc)


def _extract_sig_ts(v: VerifyConfig, headers: Mapping[str, str], raw: str) -> tuple[str, str]:
    """Pull the signature value and signed timestamp out of the request.

    Flat headers (GitHub) carry the signature directly, with the timestamp in a
    separate ``timestamp_header`` (Slack/Discord); structured ``k=v`` headers
    (Stripe ``t=..,v1=..``) carry both, selected by ``sig_field``/``ts_field``.
    """
    if v.sig_kv:
        kv = dict(p.split("=", 1) for p in raw.split(",") if "=" in p)
        return kv.get(v.sig_field, ""), kv.get(v.ts_field, "")
    ts = headers.get(v.timestamp_header.lower(), "") if v.timestamp_header else ""
    return raw, ts


def _signed_payload(template: str, body: bytes, ts: str) -> bytes | None:
    """Build the bytes that were signed. None if the template needs a ts we lack."""
    if template == "{body}":
        return body
    if "{ts}" in template and not ts:
        return None
    try:
        return template.format(body=body.decode("utf-8", "replace"), ts=ts).encode()
    except (KeyError, IndexError, ValueError):
        return None


def _ts_fresh(ts: str, max_age_s: int) -> bool:
    """True if a unix-seconds timestamp is within max_age_s of now (replay guard)."""
    try:
        return abs(time.time() - int(ts)) <= max_age_s
    except (ValueError, TypeError):
        return False


# -- Verdict + verifiers (config-driven, no provider names) --


@dataclass
class Verdict:
    """Outcome of verifying an inbound request. Default-deny."""

    authenticated: bool
    principal_id: str | None = None
    reason: str = ""
    idempotency_key: str | None = None
    allowed_kinds: frozenset[str] = field(default_factory=lambda: frozenset({"ask", "notify"}))


def _verify_hmac(source: str, headers: Mapping[str, str], body: bytes, v: VerifyConfig) -> Verdict:
    """Verify an HMAC-SHA256 webhook signature.

    Supports raw-body schemes (GitHub/Shopify, ``{body}``) and timestamped ones
    (Stripe ``{ts}.{body}``, Slack ``v0:{ts}:{body}``) via ``payload_template`` +
    the timestamp config; replay-checked when ``max_age_s`` is set. Always hashes
    raw bytes and compares timing-safe.
    """
    secret = _resolve_ref(v.secret_ref)
    if not secret:
        return Verdict(False, reason="missing_secret")
    raw = headers.get(v.header.lower())
    if not raw:
        return Verdict(False, reason="missing_signature")
    sent, ts = _extract_sig_ts(v, headers, raw)
    if v.prefix and sent.startswith(v.prefix):
        sent = sent[len(v.prefix) :]
    sent_bytes = _decode_sig(sent, v.encoding)
    if sent_bytes is None:
        return Verdict(False, reason="signature_mismatch")
    payload = _signed_payload(v.payload_template, body, ts)
    if payload is None:
        return Verdict(False, reason="missing_signature")
    if v.max_age_s is not None and not _ts_fresh(ts, v.max_age_s):
        return Verdict(False, reason="signature_mismatch")
    expected = hmac.new(secret.encode(), payload, hashlib.sha256).digest()
    if not hmac.compare_digest(expected, sent_bytes):
        return Verdict(False, reason="signature_mismatch")
    delivery = headers.get((v.delivery_id_header or "").lower()) if v.delivery_id_header else None
    return Verdict(
        True,
        principal_id=f"{INGRESS_EXT_PREFIX}{source}",
        idempotency_key=delivery or hashlib.sha256(body).hexdigest(),
        allowed_kinds=INGRESS_FLOOR,
    )


def _verify_token(source: str, headers: Mapping[str, str], body: bytes, v: VerifyConfig) -> Verdict:
    """Verify a plaintext shared-secret token sent in a header (GitLab-style)."""
    secret = _resolve_ref(v.secret_ref)
    if not secret:
        return Verdict(False, reason="missing_secret")
    sent = headers.get(v.header.lower(), "")
    if not hmac.compare_digest(secret, sent):
        return Verdict(False, reason="signature_mismatch")
    return Verdict(
        True,
        principal_id=f"{INGRESS_EXT_PREFIX}{source}",
        idempotency_key=hashlib.sha256(body).hexdigest(),
        allowed_kinds=INGRESS_FLOOR,
    )


def _verify_asymmetric(
    source: str, headers: Mapping[str, str], body: bytes, v: VerifyConfig
) -> Verdict:
    """Verify an ed25519/ecdsa signature against a configured public key.

    Uses ``cryptography`` (install the ``webhooks-asymmetric`` extra); returns
    ``scheme_not_enabled`` if it is absent. Supports timestamped payloads (e.g.
    Discord's ``{ts}{body}``) through the same template/timestamp config.
    """
    pub = _resolve_ref(v.public_key_ref)
    if not pub:
        return Verdict(False, reason="missing_secret")
    raw = headers.get(v.header.lower())
    if not raw:
        return Verdict(False, reason="missing_signature")
    sent, ts = _extract_sig_ts(v, headers, raw)
    if v.prefix and sent.startswith(v.prefix):
        sent = sent[len(v.prefix) :]
    sig = _decode_sig(sent, v.encoding)
    if sig is None:
        return Verdict(False, reason="signature_mismatch")
    payload = _signed_payload(v.payload_template, body, ts)
    if payload is None:
        return Verdict(False, reason="missing_signature")
    try:
        from cryptography.exceptions import InvalidSignature
        from cryptography.hazmat.primitives import hashes
        from cryptography.hazmat.primitives.asymmetric import ec
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
        from cryptography.hazmat.primitives.serialization import load_pem_public_key
    except ImportError:
        return Verdict(False, reason="scheme_not_enabled")
    try:
        key = load_pem_public_key(pub.encode())
    except (ValueError, TypeError):
        return Verdict(False, reason="missing_secret")  # bad key config, not a caller fault
    try:
        # isinstance narrows the load_pem union to the one key type whose verify()
        # signature we call, and rejects a key/scheme mismatch as misconfiguration.
        if v.scheme == "ed25519":
            if not isinstance(key, Ed25519PublicKey):
                return Verdict(False, reason="missing_secret")
            key.verify(sig, payload)
        else:
            if not isinstance(key, ec.EllipticCurvePublicKey):
                return Verdict(False, reason="missing_secret")
            key.verify(sig, payload, ec.ECDSA(hashes.SHA256()))
    except InvalidSignature:
        return Verdict(False, reason="signature_mismatch")
    except (ValueError, TypeError):
        return Verdict(False, reason="signature_mismatch")
    return Verdict(
        True,
        principal_id=f"{INGRESS_EXT_PREFIX}{source}",
        idempotency_key=hashlib.sha256(body).hexdigest(),
        allowed_kinds=INGRESS_FLOOR,
    )


def _verify_trust_grant(
    source: str, headers: Mapping[str, str], body: bytes, v: VerifyConfig, fed: FederationConfig
) -> Verdict:
    """Verify a P2 federation request: grant lookup + per-request HMAC over the body.

    The grant id alone is inert — a leaked id in a log or proxy is not a bearer
    credential without the per-request signature keyed by the shared secret.
    """
    grant_id = headers.get("x-repowire-grant", "")
    sig = headers.get("x-repowire-grant-sig", "")
    if not grant_id or not sig:
        return Verdict(False, reason="missing_grant")
    if grant_id in set(fed.revoked):
        return Verdict(False, reason="out_of_scope")
    grant = next(
        (g for g in fed.inbound_grants if g.grant_id == grant_id and g.direction == "inbound"),
        None,
    )
    if grant is None:
        return Verdict(False, reason="out_of_scope")
    if _is_expired(grant.expires_at):
        return Verdict(False, reason="out_of_scope")
    secret = _resolve_ref(grant.shared_secret_ref)
    if not secret:
        return Verdict(False, reason="missing_secret")
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    if not hmac.compare_digest(expected, sig):
        return Verdict(False, reason="signature_mismatch")
    # issuer_mesh_id is authenticated config, but it becomes the from_peer label —
    # constrain its charset and keep the full grant id so principals can't collide.
    origin = re.sub(r"[^A-Za-z0-9_]", "", grant.issuer_mesh_id) or "peer"
    return Verdict(
        True,
        principal_id=f"{INGRESS_FED_PREFIX}{origin}-{grant_id}",
        idempotency_key=headers.get("x-repowire-nonce") or hashlib.sha256(body).hexdigest(),
        allowed_kinds=frozenset(grant.allowed_kinds) & INGRESS_FLOOR,
    )


# Dispatch by scheme (the crypto mechanism), never by provider.
_HMAC_SCHEMES = {"hmac": _verify_hmac, "token": _verify_token}
_ASYM_SCHEMES = {"ecdsa": _verify_asymmetric, "ed25519": _verify_asymmetric}


# -- Rate limiter + dedup (lazy, no background timer) --


class _RateLimiter:
    """Per-key token bucket with lazy refill and opportunistic eviction."""

    def __init__(self) -> None:
        self._buckets: dict[str, tuple[float, float]] = {}

    def allow(self, key: str, per_min: int) -> bool:
        now = time.monotonic()
        tokens, last = self._buckets.get(key, (float(per_min), now))
        tokens = min(float(per_min), tokens + (now - last) * (per_min / 60.0))
        if tokens < 1.0:
            self._buckets[key] = (tokens, now)
            return False
        self._buckets[key] = (tokens - 1.0, now)
        if len(self._buckets) > 1024:  # lazy eviction of cold buckets
            self._buckets = {
                k: (t, ts) for k, (t, ts) in self._buckets.items() if now - ts < 3600
            }
        return True


class _Dedup:
    """Bounded FIFO set of in-flight/seen idempotency keys (lost on restart).

    Two-phase so a delivery is only treated as handled once it actually emits:
    ``claim`` reserves a key before emit (synchronously — no await between the
    check and the insert — so concurrent identical deliveries can't both pass),
    and ``release`` frees it if the emit fails so the provider's retry is
    re-attempted rather than silently dropped as a duplicate.
    """

    def __init__(self) -> None:
        self._seen: set[str] = set()
        self._order: deque[str] = deque()

    def claim(self, key: str) -> bool:
        """Reserve a key. Returns False if already claimed/seen (a duplicate)."""
        if key in self._seen:
            return False
        self._seen.add(key)
        self._order.append(key)
        if len(self._order) > _DEDUP_MAX:
            self._seen.discard(self._order.popleft())
        return True

    def release(self, key: str) -> None:
        """Free a claimed key after a failed emit so the provider's retry passes."""
        self._seen.discard(key)


# -- The peer --


class IngressPeer:
    """Verify inbound events and emit standard mesh messages through the daemon."""

    def __init__(
        self,
        ingress: IngressConfig,
        federation: FederationConfig,
        *,
        daemon_url: str = DEFAULT_DAEMON_URL,
        auth_token: str | None = None,
        display_name: str = "ingress",
        circle: str = "default",
    ) -> None:
        self._cfg = ingress
        self._fed = federation
        self._daemon_url = daemon_url.rstrip("/")
        self._auth_token = auth_token
        self._display_name = display_name
        self._circle = circle
        self._peer_id: str | None = None
        self._stopping = False
        self._client = AsyncRepowireClient(self._daemon_url, auth_token=auth_token, timeout=5.0)
        self._limiter = _RateLimiter()
        self._dedup = _Dedup()
        self._sem = asyncio.Semaphore(32)
        self._server: uvicorn.Server | None = None
        self._ws: ClientConnection | None = None

    async def start(self) -> None:
        """Register on the daemon (WebSocket) and serve the inbound listener."""
        await asyncio.gather(self._ws_loop(), self._serve_http())

    async def stop(self) -> None:
        self._stopping = True
        if self._server is not None:
            self._server.should_exit = True
        if self._ws is not None:
            await self._ws.close()
        await self._client.aclose()

    # -- Daemon WebSocket (registration + inbound replies) --

    async def _ws_loop(self) -> None:
        url = f"{_ws_url(self._daemon_url)}/ws"
        backoff = 1.0
        while not self._stopping:
            try:
                async with websockets.connect(url) as ws:
                    self._ws = ws
                    backoff = 1.0
                    connect = {
                        "type": "connect",
                        "display_name": self._display_name,
                        "circle": self._circle,
                        "backend": "claude-code",
                        "role": "service",
                        "path": "/ingress",
                    }
                    if self._auth_token:
                        connect["auth_token"] = self._auth_token
                    await ws.send(json.dumps(connect))
                    resp = json.loads(await ws.recv())
                    if resp.get("type") != "connected":
                        logger.error("ingress connect failed: %s", resp)
                        await asyncio.sleep(backoff)
                        continue
                    pid = resp.get("peer_id") or resp.get("session_id")
                    if isinstance(pid, str) and pid:
                        self._peer_id = pid
                    name = resp.get("display_name")
                    if isinstance(name, str) and name:
                        self._display_name = name
                    logger.info("ingress connected: %s", self._peer_id)
                    async for raw in ws:
                        await self._on_ws(json.loads(raw))
            except asyncio.CancelledError:
                break
            except Exception:
                if self._stopping:
                    break
                logger.warning("ingress WS lost, retry in %.0fs", backoff, exc_info=True)
                self._ws = None
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _on_ws(self, msg: dict[str, Any]) -> None:
        if msg.get("type") == "ping" and self._ws is not None:
            await self._ws.send(json.dumps({"type": "pong"}))
        # Inbound acks/replies to a federated ask arrive here. Forwarding them
        # back across the federation bridge (to the originating mesh's endpoint
        # via its outbound grant) is the remaining P2 piece — tracked separately.

    async def _serve_http(self) -> None:
        app = self._build_app()
        config = uvicorn.Config(
            app, host=self._cfg.bind_host, port=self._cfg.port, log_level="warning"
        )
        self._server = uvicorn.Server(config)
        await self._server.serve()

    def _build_app(self) -> FastAPI:
        app = FastAPI()

        @app.post("/ingress/{source}")
        async def ingress(source: str, request: Request) -> JSONResponse:
            return await self._handle(source, request)

        return app

    async def _handle(self, source: str, request: Request) -> JSONResponse:
        cfg = self._cfg.sources.get(source)
        if cfg is None:
            return _reject(404, "unknown_source")

        # Reject oversized bodies on the declared length before buffering. The
        # relay tunnel is the real size guard; this is belt-and-suspenders for
        # the pre-auth, directly-reachable path.
        clen = request.headers.get("content-length")
        if clen and clen.isdigit() and int(clen) > _BODY_MAX:
            return _reject(413, "too_large")
        body = await request.body()
        if len(body) > _BODY_MAX:
            return _reject(413, "too_large")

        headers = {k.lower(): v for k, v in request.headers.items()}
        verdict = self._verify(source, headers, body, cfg)
        if not verdict.authenticated:
            if verdict.reason == "out_of_scope":
                code = 403
            elif verdict.reason in _CONFIG_ERROR_REASONS:
                code = 500  # operator misconfiguration — not a caller auth failure
            else:
                code = 401
            return _reject(code, verdict.reason or "unauthorized")

        # Allowlist: when set, the verifier-supplied principal must match. Only
        # meaningful for trust_grant (the principal embeds the grant); for hmac/
        # token the principal is the fixed ext-<source>.
        if cfg.allow and (verdict.principal_id or "") not in cfg.allow:
            return _reject(403, "out_of_scope")

        if not self._limiter.allow(source, cfg.rate_limit_per_min):
            return JSONResponse(
                {"status": "rejected", "reason": "rate_limited"},
                status_code=429,
                headers={"Retry-After": "60"},
            )

        # Two-phase dedup: claim before emit so concurrent identical deliveries
        # don't double-emit; an already-claimed key is a duplicate replay, ACKed
        # 200 so the provider stops retrying.
        key = verdict.idempotency_key
        if key and not self._dedup.claim(key):
            return JSONResponse({"status": "duplicate"}, status_code=200)

        resp = await self._emit(source, cfg, verdict, body, headers)
        # Release the claim on a failed emit so the provider's retry is
        # re-attempted rather than silently deduplicated into a lost event.
        if key and not (200 <= resp.status_code < 300):
            self._dedup.release(key)
        return resp

    def _verify(
        self, source: str, headers: Mapping[str, str], body: bytes, cfg: IngressSource
    ) -> Verdict:
        scheme = cfg.verify.scheme
        if scheme in _HMAC_SCHEMES:
            return _HMAC_SCHEMES[scheme](source, headers, body, cfg.verify)
        if scheme in _ASYM_SCHEMES:
            return _ASYM_SCHEMES[scheme](source, headers, body, cfg.verify)
        if scheme == "trust_grant":
            return _verify_trust_grant(source, headers, body, cfg.verify, self._fed)
        return Verdict(False, reason="unknown_scheme")

    async def _emit(
        self,
        source: str,
        cfg: IngressSource,
        verdict: Verdict,
        body: bytes,
        headers: Mapping[str, str],
    ) -> JSONResponse:
        target = cfg.target
        kind = target.kind
        to_peer = target.to_peer or ""
        # Capability check: floor ∩ principal grant. Structural denial means
        # spawn/kill/schedule are simply never reachable from here.
        if kind not in (INGRESS_FLOOR & verdict.allowed_kinds):
            return _reject(403, "out_of_scope")
        if kind in ("ask", "notify") and not to_peer:
            return _reject(400, "bad_payload")

        text = _render(cfg.template, body, source)
        from_peer = verdict.principal_id or self._display_name

        async with self._sem:  # backpressure: shed load rather than queue unbounded
            try:
                if kind == "ask":
                    res = await self._client.ask(
                        to_peer, text, from_peer=from_peer, circle=cfg.circle
                    )
                    return JSONResponse(
                        {"status": "accepted", "correlation_id": res.correlation_id},
                        status_code=200,
                    )
                if kind == "notify":
                    await self._client.notify(
                        to_peer, text, from_peer=from_peer, circle=cfg.circle
                    )
                    return JSONResponse({"status": "accepted"}, status_code=200)
                # kind == "job"
                res = await self._client.create_job(
                    title=text[:120],
                    prompt=text,
                    backend=target.backend,
                    path=target.path,
                    circle=cfg.circle,
                    source_kind="ingress",
                    source_id=f"{source}:{verdict.idempotency_key}",
                    provenance={"ingress": {"source": source, "from_peer": from_peer}},
                    auto_run=target.auto_run,
                )
                return JSONResponse(
                    {"status": "accepted", "job_id": res.job_id}, status_code=200
                )
            except (DaemonConnectionError, DaemonTimeoutError):
                return _reject(503, "daemon_unavailable")
            except DaemonHTTPError as e:
                logger.warning("daemon rejected ingress emit for %s: %s", source, e)
                return _reject(502, "daemon_error")


# -- Listener helpers --


def _render(template: str | None, body: bytes, source: str) -> str:
    """Fill the emitted text from the parsed payload via str.format (never eval)."""
    if not template:
        return f"[ingress:{source}] {body.decode('utf-8', 'replace')[:1000]}"
    try:
        payload = json.loads(body or b"{}")
    except (ValueError, TypeError):
        payload = {}
    try:
        return template.format(**payload) if isinstance(payload, dict) else template
    except (KeyError, IndexError, ValueError):
        return template


def _reject(code: int, reason: str) -> JSONResponse:
    return JSONResponse({"status": "rejected", "reason": reason}, status_code=code)


# -- Entry point --


def main() -> None:
    """Entry point: repowire ingress start"""
    cfg = load_config()
    daemon = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

    if not cfg.ingress.enabled or not cfg.ingress.sources:
        print("No ingress sources configured.")
        print("Enable and add sources in ~/.repowire/config.yaml under 'ingress:'.")
        raise SystemExit(1)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    peer = IngressPeer(
        cfg.ingress,
        cfg.federation,
        daemon_url=daemon,
        auth_token=cfg.daemon.auth_token,
    )
    try:
        asyncio.run(peer.start())
    except KeyboardInterrupt:
        pass
    finally:
        asyncio.run(peer.stop())
