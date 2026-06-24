Status: proposal. Adds one new service peer (the ingress peer) for external webhook ingress (P1) and trusted cross-mesh federation (P2). P3 (adversarial/unknown parties) is explicitly out of scope. Implementation lands as a single PR; build order in the last section.

# Ingress peer

## Problem

repowire's event sources today are only peers and humans, and its only trigger is a clock (`schedule` / cron jobs). Two gaps:

1. **No external event ingress.** Triggering an agent on a webhook (PR opened, CI done, Stripe event) means hand-rolling a `POST /ask` shim per source, re-deriving signature verification, idempotency, rate-limiting, and reachability — security-critical boilerplate that is easy to get subtly wrong (skip the signature check and the endpoint is an open injection path into the mesh).
2. **No cross-scope federation.** The relay forwards only *within* one user scope (`relay/server.py`), so two coworkers on separately-authed meshes cannot pass a message between them.

## Approach

Add **one** service peer — the **ingress peer** — that owns a hardened *mechanism* (verify an inbound event, then emit a standard mesh `ask`/`notify`/`job`) and leaves routing/timing/durability to the existing primitives. It is the missing *event* trigger that sits beside `schedule` (time) and `jobs run` (manual), feeding the same dispatch pipeline.

One peer, a pluggable verifier:

- **P1 webhook:** verifier = HMAC over the raw body (the dominant webhook convention), config-driven so no provider names appear in code.
- **P2 federation:** verifier = an opaque trust-grant exchanged out-of-band + a per-request HMAC.

P3 is *not* designed for. If it ever happens it is a new verifier registered in the same dispatch table — but no seams are added now to anticipate it.

This follows the load-bearing repo philosophy: *the daemon is the only hub; transports are client-side.* The ingress peer is a client of the daemon HTTP API; verifier and policy never enter daemon or relay core. It emits a standard mesh message, so it composes with jobs, schedule, orchestrator, circles, and the human surfaces for free — it is a new *source*, not a new message type.

## The one new pattern

Every existing peer (telegram, slack) is outbound-only; only `daemon/` and `relay/` serve HTTP. The ingress peer must *receive* inbound POSTs, so it hosts a uvicorn/FastAPI listener — a new peer shape. It is mitigated by binding **localhost-only** and reaching the world through the existing relay tunnel (the relay faces the public, the peer does not), so the model stays "peers don't accept public traffic, the relay tunnels."

## Design

### Verifier (config-driven, scheme-dispatched)

A `Verifier` Protocol (`verify(req, source_cfg) -> Verdict`) with a `VERIFIERS` dict keyed by `scheme`, mirroring the relay's `_MSG_HANDLERS`. The user registers verification *metadata* per source (header, encoding, prefix, payload template, secret/public-key ref); the repo ships one generic handler per *scheme*, not per provider:

- `hmac` (default) — covers GitHub/Stripe/Slack/Shopify via config knobs; stdlib `hmac.compare_digest` over the raw bytes.
- `token` — plaintext shared secret compared against a header (GitLab-style); stdlib.
- `ecdsa`/`ed25519` — asymmetric (Discord etc.); uses `public_key_ref` and `cryptography` (already in the lockfile), shipped behind an optional extra to keep the core dependency-free.
- `trust_grant` — the P2 grant flow.

A new provider that fits an existing scheme is pure config; a genuinely new *mechanism* is one small scheme handler. Provider examples (github/stripe/slack/gitlab) ship as copy-paste docs, not code.

### Routing

One source = one narrowly-registered webhook → one fixed `to_peer` (a project peer or the `orchestrator`). The payload only fills the text template; it never selects the peer. Finer granularity = register more sources. No `when` filter, no payload-based routing, no DSL. Registration granularity is bounded by what the provider lets you select per webhook; sub-event filtering is the target peer's job — route to the orchestrator when you want an agent to triage.

### Emit and capability containment

A single `_emit` chokepoint calls `AsyncRepowireClient` (`.ask()` / `.notify()` / a new `.create_job()`). Ingress/federated principals are restricted to `ask`+`notify` (P2 may add `job`) by a plain `frozenset` check plus structural denial — the peer never wires spawn/kill/schedule/transcript paths. `from_peer` is always a namespaced id (`ext-*` for webhooks, `fed-*` for federation), never the remote's. A small daemon guard reserves those prefixes so a federated sender can never collide with or impersonate a local `repow-*` peer.

### P2 federation

An opaque `grant_` token (no signing/JWS — matches the repo's `secrets.token_urlsafe` + `compare_digest` idiom) carries scope: `{issuer_mesh_id, audience_mesh_id, direction, exposed_circle, exposed_peers, allowed_kinds, expires_at, revocation_ref, shared_secret_ref}`. Authentication is grant lookup (revocation + expiry) plus a per-request HMAC over the raw body, so a leaked grant id is inert on its own. Provenance is stamped so the recipient sees `[ask from @x via fed:bob]`, never a bare local-looking peer.

### Listener robustness

HTTP status is chosen by how providers react (they retry on non-2xx): `200` accepted; `200` for a duplicate (idempotency hit, so retries stop); `401` bad signature; `403` out-of-scope; `404` unknown source; `429` rate-limited (+`Retry-After`); `503` when the daemon is unreachable or the listener is saturated (retryable — the event is not lost); `400`/`413` for bad/oversized bodies. Two load-bearing rules: duplicate → `200`, can't-deliver → `503` ("fail loud" applied to HTTP). Rate limiting is a per-source/per-grant token bucket with lazy eviction (no sweep timer — honors "lazy repair, never poll"). Backpressure sheds load with `503`/`429` rather than building an unbounded queue. Emit is synchronous (return the real outcome) — no pre-ACK, preserving delivery truthfulness.

## Reuse

Most of this already exists. New code is deliberately one file (`repowire/ingress/bot.py`, matching the `__init__ + bot.py` footprint of every surface) plus small edits:

- Emit → `AsyncRepowireClient` (`.ask`/`.notify`/`.register_peer` exist); add `.create_job()`/`.run_job()` (the only gap).
- Config → inline `IngressConfig`/`FederationConfig` in `config/models.py` beside `TelegramConfig`.
- Crypto → `secrets.token_urlsafe` + `hmac.compare_digest`.
- Job provenance → existing `source_kind`/`source_id`/`provenance` fields on `WorkCreateRequest`.
- `uvicorn`/`fastapi` are already dependencies.

## Out of scope

P3 (adversarial federation): Sybil resistance, abuse/content policy, approval-on-first-contact, and a daemon-side scoped sub-token (today the ingress peer holds the full `daemon.auth_token`, so capability containment is peer-side — acceptable when inbound senders are untrusted but the peer is first-party, not acceptable once the peer must defend against the parties it federates with). None of these are built; the verifier dispatch table is the only thing P3 would extend.

## Build order (single PR)

1. `client.py`: add `create_job()` / `run_job()`.
2. `config/models.py`: inline `IngressConfig` / `IngressSource` / `VerifyConfig` / `IngressTarget` / `FederationConfig`; register on `Config`.
3. `repowire/ingress/bot.py`: `IngressPeer` (uvicorn listener + WS loop), verifiers, `_emit` chokepoint, dedup + rate-limit; `cli.py` `ingress` group (`start`, `grant`).
4. `registry_identity.py`: reserve `fed-`/`ext-` peer_id prefixes.
5. `relay/server.py` + `daemon/relay_client.py`: tunnel `/ingress/*` to the peer.
6. Docs (`docs/use/features/connect-webhook.md` + `mkdocs.yml`) and tests.
