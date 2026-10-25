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

All routes except `GET /health` need a bearer token. `HOSTED_TOKEN` is the
daemon's and is bound to the server-configured `HOSTED_OWNER`. `CONTROL_TOKEN`
is the relay's: it names the owner per request with `X-Repowire-Owner`, and
without one it reaches only the `/admin/*` routes. End users reach this
service through the relay, which names them; they never choose the owner.

| Request | Behavior |
| --- | --- |
| `POST /agents` with `{"id":"helper","workspace":"project"}` | Create or retrieve an agent; omit workspace for a tool-only agent |
| `GET /agents/helper` | Config, busy state, and pending operations |
| `POST /agents/helper/messages` with `{"operationId":"request-1","text":"Hello"}` | Durably accept input and return a receipt (202). Optional `source` (`from`, `type`, `fromPeerId`, `correlationId`) labels a mesh delivery in the transcript |
| `GET /agents/helper/operations/request-1` | Wait for the result; reconnect using the same operation ID |
| `GET /agents/helper/messages` | Read Pi's transcript |
| `GET /gateway` (WebSocket, daemon token) | The daemon's link for agents' mesh tool calls and run reports; one per owner, a new connection replaces the old. The upgrade names `X-Repowire-Machine`, `X-Repowire-Daemon-Id` and `X-Repowire-Version` |
| `GET /agents` | The owner's directory: each agent's state (`idle`, `working`, `failed`, `retired`), workspace, home machine, run count, tokens and estimated cost. Never wakes an agent |
| `GET /agents/helper/runs` | The run log: source, start and settle times, status, reason, tokens, estimated cost |
| `POST /agents/helper/abort` with optional `{"operationId": ...}` | Stop one run, or everything the agent is doing; it settles `unanswered` with reason `aborted` |
| `DELETE /agents/helper` | Retire: abort, then refuse new input with 410; the transcript stays readable |
| `GET /workspaces`, `GET /clients` | Workspaces with command counts, container time, the last successful snapshot and a state (`idle`, `running`, or `needs_repair` after a checkpoint failed); the connected daemon and recent disconnects, with the outbox depth |
| `GET /events` (WebSocket) | A snapshot of agents, workspaces and clients, then each change (`agent`, `run`, `workspace`, `client`, `outbox` frames). The relay holds it while someone is watching |
| `GET /admin/summary`, `/admin/owners`, `/admin/sandboxes`, `/admin/usage?days=30` | Control token without an owner: totals, agents by state, sandboxes, and daily usage across owners |

The owner's `Gateway` Durable Object holds the directory, run log and usage
ledger. Each run's model usage comes from Pi's per-response usage at list
price; each workspace command records the container wall time the workspace
measured, so waiting behind another agent's command is not charged. Costs are
estimates (`prices.ts` names the source and date). Guardrails refuse rather
than notify: a submission over the owner's daily ($2) or monthly ($20) budget,
or the agent's daily budget ($1), returns 402 `budget_exceeded`; a run is
aborted at its 25th model request, before that request is sent, and fails
with `step_limit`; more than 30
agent-originated mesh messages per owner in 10 minutes fail with
`mesh_rate_limited`. Runs started from the web use `web-` operation ids and are
not sent to the daemon's outbox.

Reusing an operation ID does not submit a second message. Reusing an agent ID
with a different workspace is a conflict. Workspace attachment is immutable.

## Prototype limits

Shell commands have no network access and time out after 45 seconds. Snapshots
are beta and expire after 30 days without use. An interrupted checkpoint fails
loudly instead of silently restoring stale files. Snapshotting each command is
intentionally simple; it costs latency and will need tuning for real workloads.
Shell tools are not declared replay-safe.

Authentication is one operator token for the daemon and one control token for
the relay, not SaaS login. The guardrails above are the only spending caps:
fixed per-owner and per-agent budgets on estimated cost, with no billing,
per-plan quotas, connector runtime, or AI Gateway configuration yet. A run in
flight can overshoot a budget by its remaining requests. Cloudflare budget
alerts only notify. Composio project context is local and ignored. No
connector credentials belong in a sandbox.

The daemon is the only mesh hub. It submits each delivery with the ask's
correlation id as the operation id and closes the ask with the answer. The
daemon reaches the agents through the `Gateway` Durable Object: it dials
`GET /gateway` and holds a hibernating WebSocket.

Results are reported, not waited on. An agent records each operation id and
its pending run row before admitting it, and when the run settles it reports
the result to the gateway; a report that arrives before its run row creates
it. The run's model usage is allocated once, when it settles, and a retry
resends that same allocation. While any report is outstanding a durable job
retries it, every minute while runs are in flight and backing off to ten
minutes while the gateway fails, so neither an eviction nor a failed report
loses a result. Retirement is checked as the agent admits each submission,
so a retire cannot race a submission into a new run. The gateway keeps each report in a durable
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
