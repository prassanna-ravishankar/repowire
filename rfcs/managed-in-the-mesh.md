# Managed in the mesh

Status: design approved by Codex review, revision 3 (2026-10-27). Phase 2 of
[managed-machine](managed-machine.md). It extends
[cross-machine circles](cross-machine-circles.md) (#516) to the `managed`
machine. Every guarantee, id rule and state machine of that RFC applies
unchanged unless this one says otherwise.

## Problem

An agent created on the Worker directly (web or relay MCP, `home_machine`
null) answers relay MCP requests (phase 1). It is invisible to the mesh,
though:
- no daemon peer can discover or ask it;
- its own mesh tools (`list_peers`, `ask_peer`, `notify_peer`) fail, because
  they are dispatched to a daemon socket that has no peer for it.

Daemon-owned hosted agents are unaffected. They are local peers of their
home daemon, so #516 already makes them reachable across machines, and they
stay there until phase 4.

## Decision

`managed` becomes one more machine in the #516 protocol, as a target and as a
source. The Worker's owner `Gateway` Durable Object plays the role a daemon
plays:
- it keeps `remote_inbound`, `remote_fences` and `remote_outbound`;
- it serves the `/remote/*` API for its agents;
- it sends `/remote/*` requests for them.

The relay carries requests between the Worker and the owner's daemon
connections. A daemon sees `managed` as an ordinary machine id.

## Identity and trust

- **Machine id.** `managed` is reserved. The relay refuses a daemon that
  connects with `daemon_id = managed`, with 409 at the upgrade.
- **Worker ingress.** The relay's Worker-originated routes (below) accept the
  control token only, compared in constant time. They are registered outside
  `managedRoute`, never accept a cookie or relay API key, and are the only
  way to obtain `from_machine = managed`.
- **Owner.** For daemon traffic, the relay names the owner from the API key,
  as today. For Worker traffic, the owner is the `X-Repowire-Owner` header on
  every route in both directions; it is never carried in a body. The Worker
  is trusted with every owner, as in phase 1.
- **Principal.** The Gateway builds a managed agent's principal from its
  directory row, `{peer_id: agent id, display_name, circle}`, and takes the
  agent id from the HostedAgent's own config over RPC. It never comes from
  tool arguments or model output.

## One ask authority

An agent is on `managed` when its directory row has `created_by != daemon`
and `home_machine` null (`onManaged` today), and it is not retired. Only
those agents are listed, targeted or allowed to send through `managed`.

Ownership is immutable until phase 4. `addAgent` refuses a create, with
409, whose `created_by` or `home_machine` would change an existing row. That
covers three cases:
- a daemon claiming a web-created agent, which today's `COALESCE` allows;
- one daemon claiming another daemon's agent;
- a web create of a daemon's agent.

An agent therefore never changes authority while its inbound or outbound
records exist. Phase 4 defines the migration.

## Circles

- Each managed agent gets a `circle` column. It is set on create (`circle` in
  `POST /agents`, defaulting to `managed`) and can be changed with
  `PATCH /agents/:id {circle}`. It must be a shared name by
  `proto.SharedCircle`'s rule, ported to TypeScript and pinned by a shared
  test vector file; a reserved name is refused with 422. Existing rows
  migrate to `managed`.
- New traffic checks the current circle. An open thread stays bound to its
  record, as in #516.

## Storage discipline (Gateway)

Durable Object input gates do not hold across an `await` on another object,
so "one DO call" is not a transaction. Every multi-statement state change is
one synchronous `ctx.storage.transactionSync(() => ...)` block, with no
`await` inside, and every terminal write is a compare-and-set on the current
state:

- **Create or fence** of `remote_inbound` and `remote_fences`. The block reads
  both tables and inserts into exactly one.
- **Report.** The `runs` upsert, metering, and the settlement of the
  matching `remote_inbound` row, keyed by both agent and operation id, happen
  in one block. The outbox write, the daemon `settled` frame, UI events and
  the settlement push all happen after the block commits.
  `Gateway.report` returns, and the agent drops its `report:<op>`, only
  after the block. A replayed report re-runs the block, and its
  compare-and-set writes repair any part that did not commit.
- **Outbound transitions** (`sent`, `uncertain`, `settled`, `delivered`).

RPCs to a HostedAgent (`submit`, `outcome`, `forget`) always run outside
these blocks. `HostedAgent.submit` calls back into `Gateway.runStarted`, so
holding the Gateway across a submit would deadlock.

## Relay

| Route or frame | Caller | Behaviour |
| --- | --- | --- |
| `machine_list` | daemon | Adds `managed` when the managed service is configured |
| `machine_request` to `managed` | daemon | Checks the path is a clean `/remote/` path. Then `POST {worker}{path}` with the control token, `X-Repowire-Owner`, `X-Repowire-From-Machine: <daemon_id>` and `X-Repowire-Deadline`, and returns the status and body as `machine_response`. A transport error or timeout is 502 or 504: the outcome is unknown, never retried. It goes through the daemon connection's existing admission pool. |
| `POST /api/v1/managed/machines` | Worker | Owner header only. Answers `{machines: [...]}`, the owner's online daemon ids, as `answerMachineList` computes them. |
| `POST /api/v1/managed/machine-request` | Worker | Body `{machine, method, path, body, deadline}` plus the owner header. Checks the path is a clean `/remote/` path and that `machine` is an online daemon of the owner (404 otherwise). Re-issues it as `remote_request` with `from_machine = managed` and answers with the target's status and body. |

Admission and binding for the Worker-originated route:

- **Capacity.** One relay-wide scheduler holds per-owner semaphores of 16
  requests in flight, inside a global cap of 256. A request that cannot take
  a slot is refused at once with 429 (`overloaded, not processed`). Slots
  belong to the HTTP request and are released when it ends, so there is no
  per-request pool and nothing to clean up beyond the semaphore.
- **Deadline.** The caller's deadline is capped at 25 s, and the relay
  applies `min(deadline, cap)`.
- **Binding.** The pending entry carries a relay-minted id. It is bound to
  the owner, the HTTP request's context, the exact target connection and the
  expected `http_response` frame. It is removed when that context is
  cancelled, when the target connection closes or is replaced (502), and
  when its answer arrives. `deliverPending`'s existing check that the sender
  is the bound target connection applies unchanged.

## Operation status (HostedAgent)

A Pi operation's state cannot be read from the agent's markers. `report:<op>`
is written before `harness.submit`, so a crash between the two leaves a
marker for an operation Pi never received. `harness.pending()` lists only
unsettled operations, and `outcome` waits for completion. Two RPCs replace
those reads. Both run in turn with admission, so no submit of the same
operation is in flight while they decide:

`status(op)` is read-only. It returns one of:

| Status | Meaning |
| --- | --- |
| `fenced` | `undelivered:<op>` exists |
| `durable` | Pi lists `op` as queued, running or parked (`pending()`) |
| `terminal` | Pi has a result for `op` (`harness.wait` answers at once with a result, since it is not pending), or `reported:<op>` exists. The result goes with it. |
| `absent` | None of the above. A leftover `report:<op>` marker from a crash before admission still counts as absent. |

`forget(op)` takes the same reads. It returns `durable` or `terminal`
unchanged. For `absent`, it withdraws any leftover marker
(`runWithdrawn`), writes `undelivered:<op>`, and returns `fenced`.

The Gateway calls both with a bound. A call that times out is unknown, and
the caller leaves the row as it is for a later pass. A timeout is never read
as admitted or as absent.

## Worker as target

The Worker's `/remote/*` routes accept the control credential, with
`X-Repowire-Owner` and `X-Repowire-From-Machine`, and forward to the owner's
Gateway. The Gateway applies #516's authority table to the agents on
`managed`, unchanged:
- **`peers`:** listed managed agents in `from.circle`, or the one matching
  `name`.
- **`ask` and `notify`:** the same circle; the target is on `managed`.
- **`asks/{id}`:** only the recorded origin may read.
- **`settled`:** accepted only from the machine recorded on an existing
  outbound row.

`store_epoch` is a random value the Gateway writes once on first use.

Dispatch:

1. A synchronous block runs create-or-fence and commits the row as
   `dispatching`, returning the existing receipt if the id is known. A
   `dispatching` row found here is recovered (below) rather than resubmitted.
2. Outside the block, the Gateway calls `HostedAgent.submit(text,
   operationId = id)`. The submit goes through the same budget and admission
   function as phase 1 requests.
3. On the answer, a synchronous block applies the compare-and-set:

   | Answer | Row becomes |
   | --- | --- |
   | Receipt (a duplicate's `accepted:false` included) | `open` (ask) or `delivered` (notify) |
   | 404, 410 | settled `not_delivered` |
   | 402 | settled `over_budget` |
   | Error with no answer | stays `dispatching` |

**Recovery of `dispatching`.** It calls `forget(id)` (above):

| `forget(id)`, bounded | Then |
| --- | --- |
| `terminal` | The row settles with that result, as a replayed report would |
| `durable` | The row becomes `open` (ask) or `delivered` (notify) |
| `fenced` | The row settles `not_delivered`. A submit still on its way is refused by the tombstone. |
| Timed out | The row stays `dispatching` for a later pass |

A notify follows the same steps.

Recovery runs lazily: on a read of that id, and on the next `/remote`
request to the owner, at most 8 rows per pass within 2 s.

**Settlement push.** After the report block commits, the Gateway makes one
`/remote/settled` attempt to the source, outside any block. If it fails, the
source's reconcile reads the result instead.

Inbound text reaches the agent framed as
`<peer-message from="@name@machine" type="ask|notify" correlation-id="id">`.

## Worker as source

`Gateway.call` routes mesh tools by the agent's authority:
- **Daemon-owned agents** keep today's socket path.
- **Agents on `managed`** are handled by the Gateway:

| Tool | Behaviour |
| --- | --- |
| `whoami` | The principal from the directory row |
| `list_peers` | Managed agents in the same circle, then the daemons through `/api/v1/managed/machines` and `/remote/peers` (16 machines, one 2 s budget covering enumeration and fan-out, `# partial:` line) |
| `ask_peer` | Resolves the target (below), mints `ask-<unix>-<32hex>`, commits `remote_outbound` (`sent`) in a block, sends `/remote/ask` outside it, and applies #516's result transitions. It returns at once with the ask id. |
| `notify_peer` | As `ask_peer`, with `/remote/notify` |
| `set_description` | The directory row, as today |

**Resolution.**
- `x@m`, with `m` a daemon: directed resolution on `m`, recording its epoch.
- `x` or `x@managed`: a managed agent in the same circle.

**Managed to managed.** Every step of the protocol short-circuits to the
same Gateway, the same code as the `/remote` handlers, with
`from_machine = managed`, the same epoch, and the origin checks: peers,
create, read, and settled. The relay route is never used, so a
managed-to-managed ask can settle and be read back.

**Spend and loops.** A reply starts a new run with a fresh 24-step limit, so
the per-run step limit does not bound an ask-and-reply chain. Two things do:
- Every outbound `ask_peer` and `notify_peer` counts against the owner's
  mesh rate limit (30 per 10 minutes) before anything is sent.
- Every reply submission passes the owner and agent budget check.

### Replies to a managed asker

A settled outbound row with `delivered = false` is handed to
`deliverReply(row)`. Daemon framing is preserved:

| Close reason | Text |
| --- | --- |
| `ack` with no reply | Nothing is submitted; the row is marked delivered (no run is spent on a bare ack) |
| `ack`, `ack_with_msg` | `[ack #id from @name@machine] <reply>` |
| `declined` | `[declined #id from @name@machine] <reason>` |
| `not_delivered`, `result_lost`, `evicted`, and the rest | `[closed #id @name@machine: <reason>] <text>` |

Order, readback first:

1. **Already delivered?** Call `status(reply-<id>)`. `durable` or
   `terminal` marks the row delivered. `fenced` cannot happen for a reply id
   and is logged. A timeout leaves the row for the next trigger. `absent`,
   including a leftover pre-admission marker, continues to step 2.
2. **Asker retired?** Mark the row delivered with disposition
   `asker_retired`. It stays visible in the agent's history.
3. **Budget.** Over budget leaves the row undelivered, retried on the next
   trigger, and shown as pending in the UI.
4. **Submit** with `operationId = reply-<id>`. A receipt or `accepted:false`
   marks the row delivered. An error leaves it for the next trigger.

Delivery is at least once, keyed by the ask id, and idempotent on the agent
through the operation id.

**Triggers.** All are request-triggered, as on a daemon, with no timer:
- the settlement push;
- the asking agent's next tool call;
- its run completion (the report);
- a read of its open asks in `/managed`;
- the next `/remote` request to the owner.

A trigger never runs inside a callback from an agent's admission.
`HostedAgent.submit` holds its admission queue while it awaits
`Gateway.runStarted`. If `runStarted` awaited `forget` or `submit` on the
same agent, the two would wait on each other. So:
- `runStarted` and `report` only note the agent as having work pending, in
  memory, and return.
- Every trigger starts one single-flight follow-up per owner. It is not
  awaited by the trigger and runs after the initiating call has returned.
- A follow-up already running absorbs later triggers.

The durable rows are the work list. A follow-up lost to an eviction is
picked up by the next trigger.

Each pass reads unsettled outbound rows not checked in the last 30 s, then
delivers settled undelivered rows: at most 8 of each, within 2 s. `expired`
and an epoch mismatch settle the row `result_lost`.

**Accepted gap.** An idle managed asker whose push was lost sees its reply
only when one of those triggers fires. A daemon asker has the same property:
its Stop and prompt hooks are requests too. The difference is that a local
agent sees a human more often. The result stays readable for retention, and
`/managed` shows the open ask. If this proves lossy, the fix is a retry
entry on the agent's existing `Lifecycle` scheduler (one Durable Object
alarm multiplexed into jobs), not a new alarm per row.

## Web and relay MCP

- **`/managed`.** It shows and edits an agent's circle and lists its open
  cross-machine asks; reading that list is a reconcile trigger.
- **Relay MCP.** The phase 1 request API is unchanged. A relay MCP ask to a
  managed agent is a phase 1 request, not a remote ask: its requester is a
  relay grant, not a machine.

## What this does not change

- Daemons: no code changes beyond the relay reserving `managed`.
- Phase 1 request semantics and the relay MCP surface.
- Offline delivery (phase 3): a daemon that is offline when a managed agent
  asks it is a definite `machine offline` failure.

## Build order

1. **Relay.** Reserve `managed`, add `machine_list` and forwarding to the
   Worker, and add the two Worker routes with their scheduler. Test with a
   fake Worker and real daemons.
2. **Worker ownership and circles.** Make ownership immutable, add the
   circle column, migration, create field and edit route, and port
   `SharedCircle` with test vectors.
3. **Worker as target.** Add the tables, `/remote` routes, the report block
   and recovery.
4. **Worker as source.** Add Gateway tool routing, outbound records, the
   managed-to-managed short circuit, `deliverReply` and the triggers.
5. **Web.** Show and edit the circle; list open asks.
6. **Live.** Run a daemon peer and a managed agent in one named circle
   against the deployed Worker and a local relay:
   - ask and reply in both directions;
   - managed to managed;
   - notify and broadcast;
   - a Worker restart mid-ask.
