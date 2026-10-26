# Cross-machine circles

Status: design approved by Codex review, revision 5 (2026-10-26). Phase 2 of the managed-machine RFC,
built for daemons first; the managed machine joins later.

## Decision

Circles are logical and per owner. Peers on different machines of the same
relay owner, in the same circle, discover and message each other.

A circle is **shared** unless it is reserved: empty, `*`, `default`, an
all-digit name, or `window-<digits>`. The rule is lexical: a circle someone
deliberately named `7` stays local too. One predicate, `proto.SharedCircle`,
is used for discovery, ask, notify and broadcast.

## Guarantees

- **Dispatch to an agent is at most once.** The transport injects an ask or
  notify at most once. A crash at the wrong moment can drop it, and it is
  never re-dispatched. Stop-hook reminders of an open ask may repeat its
  text, as they do for local asks; they are not dispatch.
- **A recorded result is never lost.** Once the target has written a
  settlement, the source can read it until retention ends.
- **A reply reaches the asker at least once, if the asker returns.** The
  reply is pushed to the asker's `peer_id`. A crash between push and mark can
  repeat it (the correlation id is the key). If that `peer_id` does not
  return, or delivery does not succeed, within the source's 7-day retention,
  the reply is never pushed. Until that deadline it stays readable through
  `wait_on_ack`; after it, the thread is gone on both sides.
- **A pending ask is not a failure.** Uncertain stays uncertain until the
  target answers for it.

## Trust model

- **Owner.** The relay authenticates a daemon by the owner's API key. All of
  an owner's daemons trust each other as much as the owner trusts them.
- **Machine.** A `daemon_id` is a label the daemon picks (its hostname), not a
  separately authenticated identity: any holder of the owner's key can
  connect as any machine id, replacing the existing connection. Requests are
  bound to the connection that sent them, so a machine speaks only as itself
  while connected, but nothing proves which host it is.
- **Principal.** The source daemon vouches for its peer: it builds
  `{peer_id, display_name, circle}` from the caller's registry entry, never
  from tool arguments.
- **No bypass.** A remote principal never bypasses circles, whatever its role
  on its own machine, and a target with a bypass role is not reachable
  remotely. A missing principal is refused (unlike local `CheckAccess`).

## Wire (relay)

| Frame | Direction | Fields |
| --- | --- | --- |
| `machine_request` | source daemon to relay | `id` (source's), `machine`, `method`, `path`, `body` |
| `remote_request` | relay to target daemon | `request_id` (relay's), `from_machine`, `method`, `path`, `body`, `deadline` |
| `machine_response` | relay to source daemon | `id`, `status`, `body` |
| `machine_list` | source daemon to relay, answered by relay | online machine ids of the owner, except the caller |

The target answers a `remote_request` with the ordinary `http_response`.

Relay rules:

- **Routing.** The target is looked up only among the same owner's
  connections. The relay refuses `machine == from_machine`, an unknown or
  offline machine (404), and a path outside `/remote/`.
- **Binding.** Every downstream id is minted by the relay, and each pending
  operation is bound to `(owner, source conn, target conn, expected frame
  type)`. `deliverPending` accepts a response only from the bound target
  connection; existing tunnel requests get the same sender check. Late
  responses, responses on a replaced connection, and duplicate active source
  ids are dropped. Either connection closing fails the operation with 502.
- **Admission.** Admission never blocks the read loop. A pool of 16 workers
  with a queue of 64 per connection takes requests; a full queue answers 429
  (`overloaded, not processed`) immediately.

## Ingress (target daemon)

`/remote/*` is not on the daemon's HTTP mux. The relay client receives the
daemon's remote handler in-process (`relay.Client.WithRemoteHandler`) and
calls it for `remote_request` frames, with `from_machine` in the request
context. No HTTP caller, header, dashboard request or tunneled `http_request`
can reach it.

Admission works as on the relay: 16 workers, a queue of 64, and an immediate
429 when full. Work still queued when its `deadline` passes is discarded
without running; the source sees a timeout, and its later read fences the id
(below).

## Ids

Ask and notify ids carry their creation time: `ask-<unix seconds>-<32 hex>`
and `notif-<unix seconds>-<32 hex>`. The target refuses to create a record
whose id is more than 6 days old or more than 5 minutes in the future
(`stale id`, a definite 4xx). Its records live 7 days, so an id can never be
created twice: a replay is either inside the retention window, where it hits
the record, or outside it, where it is refused.

Each daemon's database has a `store_epoch`, a random id written when the
database is created. Every remote response and every `/remote/settled`
callback carries it. A learns the target's epoch from directed resolution,
before dispatch, and sends it as `expected_epoch` on create. B refuses a
create whose `expected_epoch` differs from its own (`epoch_changed`, definite
4xx, nothing executed). A rejects a callback or read whose epoch differs from
the recorded one, and settles that row `result_lost`.

## Remote API

Each route has its own authority check:

| Route | Body | Authority |
| --- | --- | --- |
| `POST /remote/peers` | `{from, name?}` | current principal; shared circle; returns listed, addressable, non-bypass peers in `from.circle`, or the one matching `name` |
| `POST /remote/ask` | `{id, from, to_peer, text, expected_epoch}` | current principal; shared circle; `from.circle == target.circle`; target addressable, non-bypass |
| `POST /remote/notify` | `{id, from, to_peer, text, expected_epoch}` | same as ask |
| `POST /remote/asks/{id}` | `{from, wait}` | the recorded origin: `from_machine` and asker `peer_id` must match the record; no circle or liveness check |
| `POST /remote/settled` | `{id, status, close_reason, reply, store_epoch}` | the recorded target: A accepts it only from the machine named on an existing outbound record, and only a terminal, well-formed result |

A refusal is a definite 4xx and records nothing.

## Target (B): `remote_inbound`

Columns: `id` PK, `kind` (ask or notify), `from_machine`, `from` principal,
`to_peer_id`, `text`, `created_at`, `state` (`dispatching`, `open`,
`delivered`, `settled`),
`close_reason`, `reply`, `settled_at`.

- **States.** `dispatching` (committed before injection), then `open` (ask
  accepted by the transport), `delivered` (notify accepted), or `settled`.
  Every update is a compare-and-set on the current state, and `settled` is
  terminal: the first terminal write wins and later ones (a racing ack,
  delivery error or TTL) change nothing.
- **One write transaction.** Create and fence each run as a single
  serialized SQLite write transaction (`BEGIN IMMEDIATE`) that reads both
  `remote_inbound` and `remote_fences` and inserts into exactly one. Whichever
  commits first excludes the other.
- **Create** checks the fence first: a fence for `(id, from_machine)` returns
  that settled `not_delivered` receipt and delivers nothing. Otherwise it is
  a compare-and-insert. The same `(id, kind, from_machine, from, to_peer_id,
  text)` returns the existing receipt, whatever its state, without
  dispatching; any difference is 409. A transport rejection settles the row
  `not_delivered`.
- **Fence.** A read (`/remote/asks/{id}`) of an id with no row, whose id time
  is inside the 6-day create horizon, writes a fence `(id, from_machine,
  asker peer_id)` in `remote_fences` and answers settled `not_delivered`. An id past the horizon
  with no row answers `expired`, never `not_delivered`: its record may have
  existed and been deleted. Fences are retained 7 days like rows.
- **Recovery.** On startup, `dispatching` notifies become settled
  `uncertain` and are never re-dispatched. `dispatching` and `open` asks are
  re-registered in the tracker as pull asks with their original
  `created_at`. The agent may or may not have a `dispatching` ask. If it acks,
  the ack settles normally; otherwise the tracker TTL settles it `evicted`.
- **Settling.** A remote-origin ask settles through `RemoteAsks.Settle`, which
  commits the row first and closes the tracker entry only after the commit.
  Every terminal path calls it: bare ack, ack with reply, decline, ACP success
  or error (`completeACPAsk`, which takes the pull path for these asks),
  delivery failure, demotion, eviction. If the commit fails, the resolver
  gets an error and the ask stays open. After the commit, B attempts
  `/remote/settled` to A once.
- **Who can resolve.** Only the ask's `ToPeerID` resolves a remote-origin
  ask, on every path. `ask(reply_to=<remote-origin cid>)` is refused; reply
  with `ack` instead.
- **Retention.** Rows of any state are deleted 7 days after `created_at`, on a
  later request.

In the tracker on B, the asker's `FromPeerID` is the reserved id
`remote:<machine>:<peer_id>`. The registry refuses a local claim of any
`remote:` id, and `ForgetPeer`, quiescing and history treat it as
unregistered.

## Source (A): `remote_outbound`

Columns: `id` PK, `machine`, `store_epoch`, target `peer_id` and name, the
`from` principal (asker `peer_id`, display name, circle), `text`,
`created_at`, `state` (`sent`, `uncertain`, `settled`), `close_reason`,
`reply`, `delivered` (bool), `checked_at`. The envelope (`machine`, target,
`from`, `text`, `created_at`) is immutable after insert.

The row is authoritative. The tracker entry is only a projection of it:
local TTL eviction or pruning removes the projection but never settles the
row while B still owns the result. Rows are deleted 7 days after
`created_at`, the same basis and window as B.

In A's tracker the ask's `ToPeerID` is `remote:<machine>:<peer_id>`, so
`/asks/pending`, Stop reminders and `wait_on_ack` work by correlation id.
`wait_on_ack` keeps its signature.

Every update is a compare-and-set: `sent` may become `uncertain` or
`settled`, `uncertain` may become `settled`, and `settled` is terminal. A
callback that lands before the create response therefore wins, and the late
response cannot overwrite it.

1. A resolves `x@m` by asking `m` directly (`/remote/peers` with `name`), not
   from a discovery sample, and records `m`'s `store_epoch`. It builds `from`
   from the caller's registry entry, mints the id, commits the row (`sent`),
   and registers the local ask.
2. A sends `/remote/ask` with `expected_epoch`:
   - **2xx:** the response carries B's record state. An `open` record leaves
     the ask open; a settled one (a fence, or a retried create) is applied
     through the terminal transition below.
   - **4xx:** the ask settles as refused, with the reason.
   - **5xx, timeout or drop:** `sent` becomes `uncertain`. A never resends.
3. A result can arrive as a push (`/remote/settled`), a read, or a create
   response. All three use one transition: `UPDATE ... SET state='settled',
   close_reason, reply WHERE id=? AND state IN ('sent','uncertain')`.
4. Reply delivery is separate from settling and does not depend on a tracker
   entry. Each settled row with `delivered = false` is framed and pushed to
   the asker's `peer_id` through the reply transport, then marked
   delivered. Delivery is retried on startup and on the asker's next hook
   request, at most 8 rows per request. A row whose asker `peer_id` is not
   registered waits; at the 7-day deadline it is deleted with the row.
5. On startup, A rebuilds the tracker entry of every unsettled row from its
   envelope (text, asker name, target handle), keeping the
   original `created_at`, so `/asks/pending`, reminders and TTL behave as
   before the restart.

## Lazy reconcile (A)

Reconcile only ever runs from a request:

- **`/asks/pending` from the asker's hooks (Stop).** A selects that caller's
  outbound remote rows that are not settled and were not checked in the last
  30s, oldest `checked_at` first. It reads at most 8, single-flight per id,
  within one shared 2s deadline, outside the tracker's locks. It sets
  `checked_at` on each attempt, so every row gets its turn and no row is read
  more than twice a minute.
- **`wait_on_ack` on a remote ask.** A answers from the settled row if there
  is one. Otherwise it long-polls B's `/remote/asks/{id}` with the caller's
  timeout, capped at 20s a call, so a lost settlement is seen during the
  wait.

A read from B answers pending, settled (possibly a fresh fence), `expired`
(settles as `result_lost`), or an epoch mismatch (also `result_lost`). B
never answers "unknown". If the machine is offline the row stays as it is.

## Discovery

`list_peers` on A runs discovery only when the caller's circle is shared and
the relay is connected:

- **Fan-out.** A sends `machine_list`, then `/remote/peers` to at most 16
  machines in parallel. One 2s budget covers enumeration and fan-out.
- **Remote rows.** Each shows `name@machine`, with `machine` set to the
  canonical `daemon_id` and `remote: true`.
- **Partial results.** Machines that missed the budget, or that the cap
  left out, are named on a final `# partial: ...` line of the TSV result.
- **`circle="*"`** stays local-only.

## Addressing

`x@m` is remote when `m` is one of the owner's online machine ids other than
this one; otherwise it is an ordinary local name. If a local display name
containing `@` also matches a remote address, the call fails with 409 rather
than picking one.

## Broadcast

Broadcast fans out notifies to remote peers in the caller's shared circle,
after discovery. The result lists sent, failed and uncertain per target, and
says when the machine list was partial.

## Out of scope

- The managed machine as a source or target: it needs agent circles as
  config.
- Offline delivery: a remote target that is offline is a definite `machine
  offline` failure, never queued (the mailbox is phase 3).
- Cross-owner anything.
- Attachments and structured questions across machines.
- Moving a peer to another circle mid-thread. New traffic checks the current
  circle; an existing thread's wait and settlement stay bound to the record.
