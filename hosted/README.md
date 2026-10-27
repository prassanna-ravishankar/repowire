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
| `POST /requests` with `{"id":"rq-<32 hex>","agent":"helper","kind":"ask","requester":"relay-mcp-…","text":"…"}` | Control token only. The relay's ask or notify to an agent no daemon owns, recorded before it is submitted. A known id returns its receipt (202) before budget or retirement is checked again; a known id with a different body is 409; 404 unknown agent or one a daemon created, 410 retired, 402 over budget, 502 delivery uncertain |
| `GET /requests/rq-…?requester=…&wait=0..20` | The daemon's ask-wait contract: `pending`, or `resolved` with `close_reason` (`answered` with `reply`, `not_delivered`, or the run's unanswered reason). 404 unless `requester` sent it. A request the agent has no record of is settled `not_delivered` in turn with admission, and a later submit of it is refused; that check may hold even a `wait=0` read for up to a second |
| `GET /requests?requester=…` | That requester's unsettled asks |
| `PATCH /agents/helper` with `{"circle":"research"}` | Move an agent no daemon owns to another shared circle (422 for `default`, `*`, a number or `window-N`; 409 for a daemon's agent). `POST /agents` also takes `circle`, defaulting to `managed`. An agent never changes owner: a create that would move it between managed and a daemon is 409 |
| `POST /remote/peers`, `/remote/ask`, `/remote/notify`, `/remote/asks/{id}` | Control token only, with `X-Repowire-From-Machine` naming the daemon the relay received it from. Agents on managed as cross-machine targets, with the semantics of `rfcs/cross-machine-circles.md`: discovery in the caller's circle; asks and notifies recorded before they are submitted, at most once; reads by the recorded origin, fencing an unknown id. A run's report settles its ask in the same transaction, and the result is pushed once to the source through the relay (`RELAY_URL`) |
| `POST /remote/settled` | Control token only. The source side: the machine recorded on a managed agent's outbound ask reports it settled; any other machine is 404, and a changed store epoch settles it `result_lost` |
| `GET /admin/summary`, `/admin/owners`, `/admin/sandboxes`, `/admin/usage?days=30` | Control token without an owner: totals, agents by state, sandboxes, and daily usage across owners |

The owner's `Gateway` Durable Object holds the directory, run log and usage
ledger. Each run's model usage comes from Pi's per-response usage at list
price; each workspace command records the container wall time the workspace
measured, so waiting behind another agent's command is not charged. Costs are
estimates (`prices.ts` names the source and date). Guardrails refuse rather
than notify: a submission over the owner's daily ($2) or monthly ($20) budget,
or the agent's daily budget ($1), returns 402 `budget_exceeded`; a run is
aborted at its 25th model request, before that request is sent, and fails
with `step_limit` (if the abort fails, the request stays held and the abort is
retried, so nothing past the limit is sent); more than 30
agent-originated mesh messages per owner in 10 minutes fail with
`mesh_rate_limited`. Each run records who submitted it; only the daemon's runs
are sent to its outbox, and the relay's are answered from the run log, which
keeps each run's final text.

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
loses a result. An operation recorded but never admitted (pi has no such
submission) never ran: its pending run row is withdrawn with no report, so a
retry of the same id runs as a first admission. Retirement is checked as the agent admits each submission,
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

## Mesh tools of agents on managed

An agent no daemon owns runs its mesh tools in its owner's Gateway, as a peer
of the managed machine (`rfcs/managed-in-the-mesh.md`):
- `list_peers` shows managed agents in its circle, then each daemon's peers in
  that circle as `name@machine`. Machines that miss the 2 s budget are named on
  a `# partial:` line.
- `ask_peer` and `notify_peer` take `name` (another managed agent) or
  `name@machine`. An ask is recorded before it is sent and never resent: a send
  whose outcome is unknown answers `delivery_state=uncertain`. The reply arrives
  later as a new message (`[ack #id from @name@machine] ...`, or `[declined ...]`
  or `[closed ... : reason]`). A bare ack starts no run, and a reply to a
  retired agent is not submitted.
- The owner's mesh rate limit (30 messages per 10 minutes) and budgets apply.

Replies are reconciled and delivered on request-driven triggers (a settlement
push, the agent's next tool call, its run completion, the next cross-machine
request), never on a timer.

