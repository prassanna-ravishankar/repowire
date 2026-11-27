# Managed agents runtime

A separate deployable package in the Repowire repository: the Cloudflare Worker
that runs managed agents. This is a prototype, not the public SaaS. Only the
relay calls it: the relay serves `/managed`, proxies this API, and treats the
Worker as the `managed` machine in cross-machine circles, so daemons and their
agents reach managed agents through the relay like any other machine.

- `HostedAgent` stores a Pi Durable conversation and durable message receipts.
- `Workspace` owns an on-demand Linux sandbox (`@cloudflare/sandbox` 1.0: a
  Durable Object that runs its own Container). Several agents attach to the
  same workspace by its id; commands and saves take turns, and each command
  runs in `/workspace` with `REPOWIRE_AGENT` naming the agent. Agents without
  a workspace need no sandbox.
- The container stays up while agents work. When an agent's turn ends, the
  workspace saves `/workspace` to R2 (a directory backup) and lets the
  container stop soon after; the next container restores it. Changes a stopped
  container never saved are reported to the next command as lost. The Pi
  conversation lives independently in SQLite.
- Cloudflare's Pi lifecycle maintains recovery alarms while work runs and releases
  them when idle. There is no application polling loop.

The agent runs on pi-durable (`@earendil-works/pi-durable`) through Cloudflare's
Durable Object adapter for it, `PiHarness` from the Agents SDK
(`agents/harness/pi`). Each Agents SDK release supports one pi-durable minor
version, so the two are upgraded together: Agents SDK 0.34.1 with pi-durable
1.6.1. An upgrade is one-way. The new pi-durable migrates its tables in each
agent's storage, and an older release cannot run agents that have been migrated.

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
across sandbox save/stop/restore. Set `HOSTED_URL` to test a deployment.
These tests invoke billable model/container resources when run against Cloudflare.

Create an ignored `.dev.vars` with a random `CONTROL_TOKEN`; the smoke script
names its owner with `SMOKE_OWNER` (default `smoke`). Run `npm run dev` (Docker
is required for local shell execution; the AI binding uses Cloudflare). To
deploy, create the R2 bucket `repowire-hosted-workspaces` (`wrangler r2 bucket
create repowire-hosted-workspaces`), set `CONTROL_TOKEN` with `wrangler secret
put`, then `npm run deploy`. `wrangler dev` keeps the bucket locally.
Configure the account via `CLOUDFLARE_ACCOUNT_ID`.

## HTTP API

All routes except `GET /health` need the relay's bearer token,
`CONTROL_TOKEN`. It names the owner per request with `X-Repowire-Owner`, and
without one it reaches only the `/admin/*` routes. End users reach this
service through the relay, which names them; they never choose the owner.

| Request | Behavior |
| --- | --- |
| `POST /agents` with `{"id":"helper","workspace":"project"}` | Create or retrieve an agent; omit workspace for a tool-only agent |
| `GET /agents/helper` | Config, busy state, and pending operations |
| `POST /agents/helper/messages` with `{"operationId":"request-1","text":"Hello"}` | Durably accept input and return a receipt (202). Optional `source` (`from`, `type`, `fromPeerId`, `correlationId`) labels a mesh delivery in the transcript |
| `GET /agents/helper/operations/request-1` | Wait for the result; reconnect using the same operation ID |
| `GET /agents/helper/messages` | Read Pi's transcript |
| `GET /agents` | The owner's directory: each agent's state (`idle`, `working`, `failed`, `retired`), workspace, circle, run count, tokens and estimated cost. Never wakes an agent |
| `GET /agents/helper/runs` | The run log: source, start and settle times, status, reason, tokens, estimated cost |
| `POST /agents/helper/abort` with optional `{"operationId": ...}` | Stop one run, or everything the agent is doing; it settles `unanswered` with reason `aborted` |
| `DELETE /agents/helper` | Retire: abort, then refuse new input with 410; the transcript stays readable |
| `GET /workspaces` | Workspaces with command counts, container time, when the files were last saved and a state (`idle`, `running`, or `needs_repair` after a save failed) |
| `DELETE /workspaces/project` | Delete a workspace and its saved files on purpose. 409 while a live agent uses it or its container runs, 404 unknown. Retention also deletes a workspace once no live agent has used it past the content age |
| `GET /events` (WebSocket) | A snapshot of agents and workspaces, then each change (`agent`, `run`, `workspace` frames). The relay holds it while someone is watching |
| `POST /requests` with `{"id":"rq-<32 hex>","agent":"helper","kind":"ask","requester":"relay-mcp-…","text":"…"}` | The relay's ask or notify to an agent, recorded before it is submitted. A known id returns its receipt (202) before budget or retirement is checked again; a known id with a different body is 409; 404 unknown agent, 410 retired, 402 over budget, 502 delivery uncertain |
| `GET /requests/rq-…?requester=…&wait=0..20` | The daemon's ask-wait contract: `pending`, or `resolved` with `close_reason` (`answered` with `reply`, `not_delivered`, or the run's unanswered reason). 404 unless `requester` sent it. A request the agent has no record of is settled `not_delivered` in turn with admission, and a later submit of it is refused; that check may hold even a `wait=0` read for up to a second |
| `GET /requests?requester=…` | That requester's unsettled asks |
| `PATCH /agents/helper` with `{"circle":"research"}` | Move an agent to another shared circle (422 for `default`, `*`, a number or `window-N`). `POST /agents` also takes `circle`, defaulting to `managed`. A retired agent's id cannot be created again (410) |
| `POST /remote/peers`, `/remote/ask`, `/remote/notify`, `/remote/asks/{id}` | With `X-Repowire-From-Machine` naming the daemon the relay received it from. Agents on managed as cross-machine targets, with the semantics of `rfcs/cross-machine-circles.md`: discovery in the caller's circle; asks and notifies recorded before they are submitted, at most once; reads by the recorded origin, fencing an unknown id. A run's report settles its ask in the same transaction, and the result is pushed once to the source through the relay (`RELAY_URL`) |
| `POST /remote/settled` | The source side: the machine recorded on a managed agent's outbound ask reports it settled; any other machine is 404, and a changed store epoch settles it `result_lost` |
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
`mesh_rate_limited`. The run log keeps each run's final text, and the relay's
requests are answered from it.

Reusing an operation ID does not submit a second message. Reusing an agent ID
with a different workspace is a conflict. Workspace attachment is immutable.

## Prototype limits

Shell commands have no network access and time out after 45 seconds. Saves
go to the R2 bucket bound as `WORKSPACE_BACKUPS`; a failed save keeps the
changes in the running container for the next save and marks the workspace
`needs_repair` until one succeeds. Workspaces from before directory backups
were container snapshots on another image; their files are not carried over.
Shell tools are not declared replay-safe.

Authentication is one control token for the relay, not SaaS login. The guardrails above are the only spending caps:
fixed per-owner and per-agent budgets on estimated cost, with no billing,
per-plan quotas, connector runtime, or AI Gateway configuration yet. A run in
flight can overshoot a budget by its remaining requests. Cloudflare budget
alerts only notify. Composio project context is local and ignored. No
connector credentials belong in a sandbox.

Results are reported, not waited on. An agent records each operation id and
its pending run row before admitting it, and when the run settles it reports
the result to the owner's `Gateway`; a report that arrives before its run row
creates it. The run's model usage is allocated once, when it settles, and a retry
resends that same allocation. While any report is outstanding a durable job
retries it, every minute while runs are in flight and backing off to ten
minutes while the gateway fails, so neither an eviction nor a failed report
loses a result. An operation recorded but never admitted (pi has no such
submission) never ran: its pending run row is withdrawn with no report, so a
retry of the same id runs as a first admission. Retirement is checked as the agent admits each submission,
so a retire cannot race a submission into a new run.
`GET /agents/:id/operations/:op` remains for scripts.

The agent's mesh tools (`list_peers`, `whoami`, `notify_peer`, `ask_peer`,
`set_description`) run through the owner's `Gateway` and the relay, as that
agent on the `managed` machine (`rfcs/managed-in-the-mesh.md`). Mesh tools are not
replay-safe, so an interrupted run does not send a message twice. `source` is a transcript label, not an authorization; peer text arrives
framed as an untrusted `<peer-message>`. SaaS identity should name the owner before multi-user
access; Composio sessions must derive their user ID from that authenticated owner.

## Mesh tools

An agent runs its mesh tools in its owner's Gateway, as a peer of the managed
machine (`rfcs/managed-in-the-mesh.md`):
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

