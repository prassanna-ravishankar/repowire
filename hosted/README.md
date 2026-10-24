# Hosted agents spike

A separate deployable package in the Repowire repository. This is a private,
single-owner runtime prototype, not the public SaaS or a mesh transport yet.

- `HostedAgent` stores a Pi Durable conversation and durable message receipts.
- `Workspace` owns an on-demand Linux sandbox. Several agents may attach to the
  same workspace; commands serialize. Agents without a workspace need no sandbox.
- A shell command checkpoints its filesystem and stops the sandbox. A later call
  restores that snapshot. The Pi conversation lives independently in SQLite.
- Cloudflare's Pi lifecycle maintains recovery alarms while work runs and releases
  them when idle. There is no application polling loop.

## Run

Use Node 24.11+ (or a supported newer release):

```sh
npm ci
npx wrangler types
npm run check
npm test
```

With the dev server running, exercise the HTTP boundary with
`node --env-file=.dev.vars scripts/smoke.mjs`. Add `--model` for a real model
turn and durable message deduplication (requires Workers AI access). After a
redeploy, pass `--recall <agent-id>` using the ID printed by the model test to
verify conversation recovery. Pass `--workspace` to test two agents sharing files
across sandbox checkpoint/stop/restore. Set `HOSTED_URL` to test a deployment.
These tests invoke billable model/container resources when run against Cloudflare.

Create an ignored `.dev.vars` with a random `HOSTED_TOKEN` and a stable
`HOSTED_OWNER`. The owner is server-configured; callers cannot override it.
Run `npm run dev` (Docker is required for local shell execution; the AI binding
uses Cloudflare). To deploy, set those two values with `wrangler secret put`,
then `npm run deploy`. Configure the account via `CLOUDFLARE_ACCOUNT_ID`.

## HTTP API

All routes except `GET /health` require `Authorization: Bearer <HOSTED_TOKEN>`.

| Request | Behavior |
| --- | --- |
| `POST /agents` with `{"id":"helper","workspace":"project"}` | Create or retrieve an agent; omit workspace for a tool-only agent |
| `GET /agents/helper` | Config, busy state, and pending operations |
| `POST /agents/helper/messages` with `{"operationId":"request-1","text":"Hello"}` | Durably accept input and return a receipt (202) |
| `GET /agents/helper/operations/request-1` | Wait for the result; reconnect using the same operation ID |
| `GET /agents/helper/messages` | Read Pi's transcript |

Reusing an operation ID does not submit a second message. Reusing an agent ID
with a different workspace is a conflict. Workspace attachment is immutable.

## Prototype limits

Shell commands have no network access and time out after 45 seconds. Snapshots
are beta and expire after 30 days without use. An interrupted checkpoint fails
loudly instead of silently restoring stale files. Snapshotting each command is
intentionally simple; it costs latency and will need tuning for real workloads.
Shell tools are not declared replay-safe.

Authentication is one operator token, not SaaS login. There is no billing,
per-user quota, connector runtime, AI Gateway configuration, or daemon/relay
adapter yet. Composio project context is local and ignored. No connector
credentials belong in a sandbox. Budget alerts are notifications, not hard caps.

The next integration is a transport adapter into the existing daemon, preserving
`peer_id` routing and message provenance. The hosted runtime must not become a
second mesh hub. SaaS identity should replace the fixed owner before multi-user
access; Composio sessions must derive their user ID from that authenticated owner.
