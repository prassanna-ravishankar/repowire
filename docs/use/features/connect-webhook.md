# Webhooks & ingress

The ingress peer turns external events into mesh messages. A verified inbound
webhook (a PR opened, CI finished, a Stripe event) becomes a standard `ask`,
`notify`, or durable `job` aimed at one of your peers — so an outside system can
trigger your agents the same way `schedule` triggers them on a clock.

It is the inbound counterpart to the Telegram and Slack surfaces: one service
peer that verifies a request, then emits a normal mesh message. It owns the
security envelope (signature verification, idempotency, rate-limiting); routing,
timing, and durability are composed from the primitives you already have.

## When to use it

- Trigger a project peer when an event fires in GitHub/Stripe/CI/etc.
- Route events to your `orchestrator` and let it triage and dispatch.
- Open a durable job per event (`kind: job`) so the work is tracked, retried, and reported.
- Bridge two trusted, separately-authed meshes (cross-mesh federation).

## Requirements

- macOS or Linux, Python 3.10+ (same as the daemon).
- Public reachability is provided by the **relay** — the ingress peer binds
  localhost only and the relay tunnels `/ingress/*` to it, so you never open an
  inbound port. Enable the relay with `repowire setup --relay`.

## Setup

Add an `ingress:` block to `~/.repowire/config.yaml`. Each **source** is one
narrowly-registered webhook: you register its path (`/ingress/<key>`) with the
provider, and it emits to one fixed peer.

```yaml
ingress:
  enabled: true
  sources:
    gh-pr:
      verify:
        scheme: hmac
        header: X-Hub-Signature-256
        prefix: "sha256="
        encoding: hex
        secret_ref: GH_WEBHOOK_SECRET     # env/keychain name — never the raw secret
        delivery_id_header: X-GitHub-Delivery
      target:
        kind: ask                          # ask | notify | job
        to_peer: code-reviewer             # a project peer, or "orchestrator"
      template: "PR #{number} {action}: {pull_request[title]}"
```

Then start the peer:

```bash
export GH_WEBHOOK_SECRET=...    # the secret you also paste into GitHub's webhook
repowire ingress start
```

Point the provider's webhook at `https://<your-relay>/ingress/gh-pr` (or your own
tunnel if you run the listener directly). On each delivery the peer verifies the
signature, renders `template` from the payload, and opens an ask to `to_peer`.

### Verifier is config, not code

There are no provider names in Repowire. You describe how a provider signs its
requests in the `verify:` block; one generic handler per **scheme** reads it.
Common providers:

```yaml
# GitHub — HMAC-SHA256 hex over the raw body
verify: { scheme: hmac, header: X-Hub-Signature-256, prefix: "sha256=", encoding: hex, secret_ref: GH_SECRET }

# Shopify — HMAC-SHA256 base64
verify: { scheme: hmac, header: X-Shopify-Hmac-SHA256, encoding: base64, secret_ref: SHOPIFY_SECRET }

# GitLab — plaintext shared token, no HMAC
verify: { scheme: token, header: X-Gitlab-Token, secret_ref: GITLAB_TOKEN }

# Stripe — timestamped HMAC; signature header is `t=..,v1=..`, replay-checked
verify: { scheme: hmac, header: Stripe-Signature, prefix: "", sig_kv: true, ts_field: t, sig_field: v1,
          payload_template: "{ts}.{body}", max_age_s: 300, secret_ref: STRIPE_SECRET }

# Slack — timestamped HMAC; timestamp in a separate header
verify: { scheme: hmac, header: X-Slack-Signature, prefix: "v0=", timestamp_header: X-Slack-Request-Timestamp,
          payload_template: "v0:{ts}:{body}", secret_ref: SLACK_SECRET }

# Discord — Ed25519 over timestamp+body, verified with a public key
verify: { scheme: ed25519, header: X-Signature-Ed25519, prefix: "", encoding: hex,
          timestamp_header: X-Signature-Timestamp, payload_template: "{ts}{body}", public_key_ref: DISCORD_PUBKEY }
```

`scheme: hmac` (raw-body and timestamped) and `scheme: token` are stdlib and
shipped by default. `ecdsa`/`ed25519` use `public_key_ref` and the
`cryptography` library — install the `repowire[webhooks-asymmetric]` extra.

## Targets and routing

`target.to_peer` is fixed per source — the payload fills the text, it never
selects the peer. For finer routing register more sources, or route to the
`orchestrator` and let an agent decide:

```yaml
    ci-failed:
      verify: { scheme: hmac, header: X-Signature, secret_ref: CI_SECRET }
      target: { kind: job, to_peer: orchestrator, backend: codex, auto_run: true }
      template: "CI failed on {branch}: {message}"
```

A `kind: job` source is the **event trigger** for durable jobs, beside cron
(time) and `repowire jobs run` (manual) — it feeds the same tracked-work
pipeline with `source_kind: ingress` provenance.

## Cross-mesh federation (P2)

Two trusting meshes exchange an opaque grant out-of-band:

```bash
repowire ingress grant   # prints a grant_id + shared secret to share securely
```

The receiving mesh stores the grant under `federation.inbound_grants` (scoped to
a circle, peers, and message kinds, with an expiry). A federated request carries
`X-Repowire-Grant` + `X-Repowire-Grant-Sig` (an HMAC over the body); the grant id
alone is inert. Cross-mesh messages render as `[ask from @x via fed:<mesh>]` and
can never impersonate a local peer.

## Limits

- Capability is restricted to `ask`/`notify`/`job`; spawn/kill/schedule are never reachable from ingress.
- Registration granularity is bounded by what a provider lets you select per webhook (e.g. GitHub scopes to the `pull_request` event, not the `opened` action). Sub-event filtering is the target peer's job.

## Troubleshooting

- `401 signature_mismatch` — the configured `secret_ref` value does not match the provider's secret, or the wrong `encoding`/`prefix`.
- `404 unknown_source` — the path segment does not match a key under `ingress.sources`.
- `503 daemon_unavailable` — the daemon is not running; the provider will retry.
- Duplicate deliveries return `200 {"status":"duplicate"}` and do not re-emit.

## See also

- [Jobs and schedules](../../concepts/jobs-and-schedules.md) — the trigger family ingress joins.
- [Relay access](relay-access.md) — public reachability without inbound ports.
- [Orchestrator coordination](../workflows/orchestrator-coordination.md) — routing events to an agent.
