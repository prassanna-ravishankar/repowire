# Hosted agents spike

A separate deployable package in the Repowire repository. This is a private,
single-owner runtime prototype, not the public SaaS. The daemon reaches it as the
`hosted` backend: `spawn_peer(backend="hosted")` registers a managed peer whose
`peer_id` is the agent id here, and mesh deliveries arrive as messages.

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
| `POST /agents/helper/messages` with `{"operationId":"request-1","text":"Hello"}` | Durably accept input and return a receipt (202). Optional `source` (`from`, `type`, `fromPeerId`, `correlationId`) labels a mesh delivery in the transcript |
| `GET /agents/helper/operations/request-1` | Wait for the result; reconnect using the same operation ID |
| `GET /agents/helper/messages` | Read Pi's transcript |
| `GET /gateway` (WebSocket) | The daemon's link for agents' mesh tool calls; one per owner, a new connection replaces the old |

Reusing an operation ID does not submit a second message. Reusing an agent ID
with a different workspace is a conflict. Workspace attachment is immutable.

## Prototype limits

Shell commands have no network access and time out after 45 seconds. Snapshots
are beta and expire after 30 days without use. An interrupted checkpoint fails
loudly instead of silently restoring stale files. Snapshotting each command is
intentionally simple; it costs latency and will need tuning for real workloads.
Shell tools are not declared replay-safe.

Authentication is one operator token, not SaaS login. There is no billing,
per-user quota, connector runtime, or AI Gateway configuration yet. Composio project context is local and ignored. No connector
credentials belong in a sandbox. Budget alerts are notifications, not hard caps.

The daemon is the only mesh hub. It submits each delivery with the ask's
correlation id as the operation id and closes the ask with the answer. The
daemon reaches the agents through the `Gateway` Durable Object: it dials
`GET /gateway` and holds a hibernating WebSocket.

Results are reported, not waited on. An agent records each operation id
before admitting it, and when the run settles it reports the result to the
gateway. Each wake of the agent re-arms reports still outstanding, so an
eviction mid-run still reports. The gateway keeps each report in a durable
outbox, sends it to the daemon, and resends everything unacknowledged on
every connect. The daemon settles a report once and acknowledges it, so a run
that finishes while the daemon is down is delivered when it reconnects.
`GET /agents/:id/operations/:op` remains for scripts.

The agent's mesh tools (`list_peers`, `whoami`, `notify_peer`,
`ask_peer`, `set_description`) send a call frame over it, and the daemon runs
the call as that agent's peer and answers with a result frame. Protocol pings
do not wake the object. With no daemon connected the tools fail at once; a call
unanswered after 60 seconds fails and may still have run. Mesh tools are not
replay-safe, so an interrupted run does not send a message twice. `source` is a transcript label, not an authorization; peer text arrives
framed as an untrusted `<peer-message>`. SaaS identity should replace the fixed owner before multi-user
access; Composio sessions must derive their user ID from that authenticated owner.
