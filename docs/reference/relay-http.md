# Relay HTTP API

Routes served by the relay server (`repowire relay start`). Daemon routes reached through the relay tunnel are documented in [HTTP API](http-api.md); the OAuth MCP surface is in [Relay MCP and OAuth](relay-mcp.md).

## Authentication

Browser routes use the `rw_token` cookie set by the landing page. API callers may send `Authorization: Bearer <relay key>` instead. The relay key identifies the user as `token-<sha256 of the key>`; that id is the owner of managed agents.

Cookie-authenticated writes (`POST`, `DELETE`) are refused with **403** when an `Origin` header names a different host.

## Relay routes

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /` | none | Landing page; redirects to `/dashboard` when the cookie is valid. |
| `POST /auth` | form `token` | Sets the cookie when a daemon is connected for the key. |
| `GET /dashboard` | cookie | Dashboard static export. |
| `GET /managed` | cookie | Managed agents page (`web/out/managed.html`). Signed-out visitors are redirected to `/`. |
| `GET /admin` | cookie, admin | Admin page (`web/out/admin.html`). Non-admins get **403**. |
| `GET /health` | none | `{status, connected_daemons}`. |
| `GET /events/stream` | cookie | SSE stream tunneled from the daemon. |
| `POST /api/v1/register` | none | Mints a relay key. |
| `GET /api/v1/daemons` | `X-API-Key` | Daemons connected for the key. |
| `POST/GET /api/v1/share`, `DELETE /api/v1/share/{id}` | `X-API-Key` | Session share links. |
| `/ws/relay` | `api_key` query | Daemon tunnel WebSocket. |

## Managed agents

Managed agents run on a Cloudflare Worker. The relay authenticates the caller and calls the Worker control API with its control token, naming the caller as owner. The Worker never accepts an owner from a browser.

Errors are `{"error": "..."}`. Until `REPOWIRE_MANAGED_URL` and `REPOWIRE_MANAGED_CONTROL_TOKEN` are set, routes that need the Worker return **503** with a message naming those variables. A Worker that cannot be reached gives **502**.

| Route | Behavior |
| --- | --- |
| `GET /api/v1/managed/me` | `{user_id, admin, managed}`. Works without the Worker. |
| `GET /api/v1/managed/agents`, `POST /api/v1/managed/agents` | Proxied to the Worker. |
| `GET /api/v1/managed/agents/{id}`, `DELETE /api/v1/managed/agents/{id}` | Proxied. `DELETE` retires the agent. |
| `GET /api/v1/managed/agents/{id}/messages`, `POST .../messages` | Proxied. A retired agent answers **410**. |
| `GET /api/v1/managed/agents/{id}/runs` | Proxied. |
| `POST /api/v1/managed/agents/{id}/abort` | Proxied. |
| `GET /api/v1/managed/workspaces` | Proxied. |
| `GET /api/v1/managed/clients` | See below. |
| `DELETE /api/v1/managed/apps/{grant_id}` | Revokes an OAuth grant the caller owns; **404** otherwise. |
| `GET /api/v1/managed/events` | SSE, see below. |

Proxied routes keep the Worker path below `/api/v1/managed`, forward the query string and a JSON body of up to 1 MiB, and return the Worker's status and body unchanged.

### Clients

```json
{
  "daemons": [{"daemon_id": "laptop", "connected_at": "...", "connected": true}],
  "gateway": [{"kind": "daemon", "machine": "...", "daemon_id": "...", "version": "...", "connected_at": "...", "connected": true}],
  "apps": [{"grant_id": "...", "client_id": "...", "client_name": "...", "scope": "agents:read", "expires": "...", "active": true}],
  "outbox": 0
}
```

`daemons` are relay tunnel connections, `gateway` and `outbox` come from the Worker's `GET /clients`, and `apps` are OAuth grants that are not revoked (`active` is false until the client has exchanged its first token). If the Worker call fails or is unconfigured, `gateway` is empty and `gateway_error` carries the reason.

### Events

`GET /api/v1/managed/events` is a Server-Sent Events stream for the caller's agents.

- Worker frames arrive unchanged as one `data:` line each (`snapshot`, `agent`, `run`, `workspace`, `client`, `outbox`).
- Relay frames use `event: relay`:
  - `{"type":"daemons","daemons":[...]}` first, listing relay tunnel connections.
  - `{"type":"daemon","daemon":{daemon_id, connected_at, connected}}` when a relay tunnel connects or disconnects.
  - `{"type":"gateway","connected":true|false,"error"?}` when the Worker socket opens or drops. The relay reconnects with backoff up to 30 s, and the Worker sends a fresh `snapshot` on each connect.
- `: keepalive` comments every 15 s.
- `event: error` followed by end of stream when the client falls more than 64 frames behind. `EventSource` reconnects on its own.

The relay opens one Worker `/events` WebSocket per owner when the first stream subscribes and closes it when the last one leaves. Nothing polls.

## Admin

Admin routes require the caller's user id in `REPOWIRE_ADMIN_USERS`; others get **403**.

| Route | Behavior |
| --- | --- |
| `GET /api/v1/admin/summary` | Worker summary plus `relay: {daemons_connected, users_with_daemons, oauth_grants_active}`. |
| `GET /api/v1/admin/owners` | Proxied. |
| `GET /api/v1/admin/sandboxes` | Proxied. |
| `GET /api/v1/admin/usage?days=` | Proxied. |
| `GET /api/v1/admin/costs?days=&refresh=1` | Estimate and actual spend, below. |

### Costs

```json
{
  "estimate": {"days": [{"day", "model_usd", "container_usd", "do_usd", "total_usd"}], "month_usd": 0, "projected_usd": 0},
  "actual": null,
  "alerts": [15, 30],
  "as_of": "..."
}
```

`estimate` sums the Worker's `/admin/usage` across owners per day, at gross list price (before allowances, without the $5 base). It covers at least the current month (`days` defaults to 30, max 90). `do_usd` is 0 because the Worker does not meter Durable Objects. `projected_usd` is `month_usd × days_in_month / day_of_month`.

`actual` is set when `REPOWIRE_CF_ACCOUNT_ID` and `REPOWIRE_CF_ANALYTICS_TOKEN` (Account Analytics Read) are configured. It is Cloudflare's account usage for the current month, priced after the Workers Paid allowances:

```json
{
  "month": {"base_usd": 5, "ai_usd", "containers_usd", "do_usd", "workers_usd", "total_usd"},
  "projected_usd": 0,
  "days": [{"day", "ai_usd", "containers_usd", "do_usd", "workers_usd", "total_usd", "units": {}}],
  "unavailable": {"<dataset>": "<error>"},
  "as_of": "..."
}
```

Each day carries the cost of the usage that crossed a monthly allowance that day, so the days add up to the month (plus the base). Workers AI uses the daily 10,000-neuron allowance. Sources:

| Driver | GraphQL dataset | Fields |
| --- | --- | --- |
| Workers AI | `aiInferenceAdaptiveGroups` | `sum.totalNeurons` |
| Containers | `containersUsageAdaptiveGroups` | `sum.cpuTimeSec`, `allocatedMemory`, `allocatedDisk`, `txBytes` |
| DO requests | `durableObjectsInvocationsAdaptiveGroups` | `sum.requests` |
| DO duration | `durableObjectsPeriodicGroups` | `sum.activeTime` (µs, billed at 128 MB) |
| DO storage | `durableObjectsStorageGroups` | `max.storedBytes` |
| Workers | `workersInvocationsAdaptive` | `sum.requests`, `sum.cpuTimeUs` |

Each dataset is queried separately. One that fails (missing permission, unknown field) is listed in `unavailable` and contributes nothing, and the rest are still priced. The `totalNeurons`, `activeTime`, and `cpuTimeUs` field names have not been confirmed against a live account. DO rows written are not priced.

The result is cached in memory for one hour and refetched on the first request after that, or on `refresh=1`.
