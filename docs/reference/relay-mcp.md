# Relay MCP

The relay exposes a separate, authenticated Streamable HTTP MCP server at
`https://relay.repowire.io/mcp` when OAuth is enabled. It connects remote MCP
clients to existing local agents through the daemon's outbound WebSocket tunnel.
The daemon remains the routing and delivery authority. Its administrative,
localhost-only `/mcp` endpoint is still blocked by the tunnel.

## Connect

Update the local Repowire binary, restart its daemon when ongoing work permits,
and enable the relay with `repowire setup --relay`. Keep the intended agent and
its daemon running. A daemon with the `ask_pull_delivery` health capability is
required for tracked asks; an older daemon receives no task and returns an
update instruction.

Add the relay MCP URL in the client's remote MCP connection UI and choose OAuth.
The client opens the relay consent page. An existing relay browser login can be
reused; otherwise enter the relay secret **on that page**, then approve access.
The client receives separate OAuth credentials, never the relay secret.

For the complete skill bundle, install the portable package under
`plugins/repowire-remote`. Claude Code users can install the companion plugin:

```text
/plugin marketplace add prassanna-ravishankar/repowire
/plugin install repowire-remote@repowire
```

Authenticate the plugin server in Claude Code's `/mcp` UI. Client installation,
authentication prompts, and write confirmations remain client-controlled.
For a self-hosted relay, replace the URL in the canonical `mcp.json` and run
`scripts/package-remote-plugin.py` to regenerate the Claude adapters.

The existing `repowire` plugin remains the local CLI/MCP skill pack. The remote
pack contains conversation, delegation, review, and planning skills that work
without local shell access. Both portable and Claude formats share its `skills/`
directory. Generated Claude JSON adapters are checked with:

```bash
python3 scripts/package-remote-plugin.py --check
claude plugin validate ./plugins/repowire-remote
```

## Tools

| Tool | Purpose | Required scope |
| --- | --- | --- |
| `list_machines()` | Connected machines, including exact daemon IDs | `agents:read` |
| `list_agents(daemon_id, circle?)` | Online, addressable agents and current work | `agents:read` |
| `ask_agent(daemon_id, peer_id, message)` | Send a task/question requiring a reply | `agents:write` |
| `send_message(daemon_id, peer_id, message)` | Send information without requiring a reply | `agents:write` |
| `get_reply(daemon_id, request_id, wait_seconds?)` | Retrieve a reply; optionally wait 0–20 seconds | `agents:read` |
| `list_requests(daemon_id)` | Outstanding asks from this app connection | `agents:read` |

Select a machine and the exact `peer_id` returned by discovery. Display names are
not accepted as message targets. A machine ID never silently falls back to
another connection. Permissions cover all machines sharing the approved relay
secret. Per-machine or per-agent grants are not provided in this version.

`ask_agent` returns `daemon_id`, `peer_id`, `request_id`, and `status: pending`.
Save the IDs and call `get_reply`. A pending wait leaves the work running. A
resolved request can contain a reply, a decline, or a close reason without text;
closure alone is not proof of successful work. `send_message` returns the daemon's
actual delivery state, including queued delivery where applicable.

If a send times out or its connection drops, delivery may already have happened.
Inspect `list_requests` before considering a retry. This lists outstanding work
only, so absence is not evidence of non-delivery. There is no automatic resend or
exactly-once guarantee. Every new ask is a new task; include prior context in
follow-ups. Tools expose no spawn, kill, shell, file-read, broadcast, or approval
capability.

Requests live in the local daemon's in-memory ask tracker and follow its retention
rules. A daemon restart can lose them. OAuth refresh preserves the connection's
asker identity; a newly authorized grant receives a different identity and cannot
retrieve the old grant's replies. This version does not push unsolicited agent
messages into an idle ChatGPT conversation.

## OAuth contract

The implementation uses `go-oauth2/oauth2/v4` for authorization codes, PKCE,
exchange validation, expiry, and refresh rotation, backed by indexed, transactional SQLite
storage. MCP uses the official Go SDK in stateless JSON-response mode.

- Discovery: `/.well-known/oauth-protected-resource/mcp` (also the root metadata
  path) and `/.well-known/oauth-authorization-server`.
- Public-client dynamic registration: `POST /oauth/register`, with exact HTTPS
  redirect URIs (HTTP loopback callbacks allow a new ephemeral port, with all other components fixed), and
  `token_endpoint_auth_method: none`. Client registration is rate-limited.
- Authorization: `/oauth/authorize`, response type `code`, PKCE `S256` only,
  browser-bound consent, and `resource` equal to the canonical issuer plus `/mcp`.
- Token exchange: `POST /oauth/token` with URL-encoded parameters, including
  `client_id` and `resource`. Supports `authorization_code` and `refresh_token`.
- Scope: `agents:read` is required; add `agents:write` for messaging. Omitted scope
  defaults to read-only. Refresh may narrow scope; reconnect to add messaging access.
- Access tokens expire after 15 minutes. Refresh tokens rotate on every exchange.
  Grants have an absolute 30-day lifetime, after which consent is required again.
  Refresh works while local daemons are offline.
- Reusing a consumed refresh token revokes its entire grant, including all issued
  access tokens. Clients must serialize refresh requests. If a refresh response
  is lost after commit, reconnect rather than replaying the old refresh token.
- Revocation: `POST /oauth/revoke` with `client_id` and a token, or use
  `/oauth/connections` after signing in to the relay. Revocation rejects subsequent
  requests; it does not cancel tasks already delivered to an agent.
- Only credential hashes are persisted. Grants and token rotation commit before
  credentials are returned. OAuth database failures fail closed.

This version supports dynamic registration, not Client ID Metadata Document
fetching or confidential-client secrets. Use DCR when configuring the client.
Authorization codes expire after one minute; consent forms expire after ten.
OAuth errors and token responses are not cacheable, and tokens are never placed
in the MCP URL or plugin package.

## Relay identity and migration

Relay secrets are bearer capabilities. The full secret's SHA-256 digest defines
its namespace; possession of that exact secret connects the browser, daemon,
and OAuth grants. First authorization requires a daemon with the matching secret
online. No external identity provider or email account is required.

Older relays derived fallback identities from the last eight characters. The
full-secret change prevents different secrets sharing that suffix from sharing a
namespace. Public key registration now always generates a fresh capability;
caller-supplied `user_id` cannot select an existing owner's identity or retrieve
its secret. Existing configured secrets keep working when daemons reconnect.
In-memory share links must be recreated after a relay restart as before.

## Self-hosting

Set both variables when starting `repowire relay start`:

```bash
REPOWIRE_RELAY_OAUTH_ISSUER=https://relay.example.com \
REPOWIRE_RELAY_OAUTH_DB=/data/oauth.db \
repowire relay start --host 0.0.0.0 --port 8000
```

The issuer must be a stable HTTPS origin. HTTP loopback origins are allowed for
local development. It is never derived from request Host or forwarded headers.
The database is bound to that issuer; changing it requires a new database and
new client authorization. Omit the issuer to leave relay MCP disabled.

Persist `/data` across container replacement and restrict database access. The
Helm chart provisions a retained PVC and uses one replica with a Recreate strategy.
It rejects multiple replicas with OAuth enabled: live tunnel ownership and the
SQLite store are not a distributed service. Back up the database as sensitive
state. Losing it invalidates app registrations, grants, and refresh tokens.

The relay must be reachable by the MCP client, and the agent machine must maintain
its outbound relay connection. An offline machine produces a tool error; token
refresh itself does not depend on that machine being online.

Registration stores at most 4,096 unused clients with a 24-hour lifetime; the oldest
unused registration is evicted when full. Clients with an activated grant remain
registered for 90 days from their latest token exchange. If a cached registration
expires, the authorization error directs users to remove and re-add the connector. Total redirect URI text
is limited to 4 KiB per registration. Consent forms carry signed, browser-bound
state using Gorilla securecookie; merely opening a form writes no pending records.
Abandoned authorization codes and inactive grants expire after one minute.
Token checks perform indexed, read-only lookups. Expiry cleanup runs on writes.

Registration is rate-limited per source IP (five initial requests, replenishing
one per minute), with a separate global ceiling. Behind an access-controlled
Cloudflare proxy, set `REPOWIRE_RELAY_TRUST_CF_CONNECTING_IP=true` (Helm:
`oauth.trustCloudflareIP=true`) only when the origin rejects direct traffic and
its trusted ingress preserves or sanitizes that header. Otherwise headers are
ignored and the socket address is used; clients behind one proxy share a bucket.
