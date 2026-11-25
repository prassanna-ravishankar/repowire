# Message ids, pull delivery, and ask delegation

Status: implemented (2026-11-20). Smallest coherent version; everything is
additive and the current push, ask and ack behaviour stays the default.

## Decision

Messages to agent peers become addressable objects:

1. **Every message has a durable id.** A notify, an ask, or a reply delivered
   to an agent peer is recorded once per recipient as a `msg-<hex8>` row, and
   the wire frame carries that `message_id`.
2. **A peer can pull.** It can list its messages, peek at one, or fetch one
   (fetch marks it read), by id.
3. **A peer can choose pull delivery.** With `message_delivery: pull` its pane
   receives a one-line nudge naming the message id instead of the body.
4. **An ask can be delegated.** Its current recipient hands it to another
   peer. The correlation id is kept, the reply goes to the original asker
   exactly once, and the hop is recorded.

This is the mesh thesis applied to messages: peers are persistent and
directly addressable, so the things they send each other should be too.

## What exists today

| Id | Minted by | Lives in | Pull? |
| --- | --- | --- | --- |
| `notif-delivery-<hex8>` | router, per notify send | the frame and its trace rows | no; the body is not stored for agent recipients |
| `ask-<hex8>` correlation id | `AskTracker.Register` | in-memory tracker (24h TTL) plus the `delivery_traces` ledger | only the asker, for the reply (`reply_delivery: pull`, `wait_on_ack`) |
| `ask-delivery-<hex8>` | router, per ask frame | the frame | no |
| `inb-<hex8>` inbox item | `Inbox.Store` | `inbox_items` (durable) | yes: `GET /inbox`, `/inbox/{id}`, read, reply. Human peer only |
| queued delivery id | `EnqueueDelivery` | `queued_deliveries` (24h TTL, 50 per peer) | drained on connect, then replayed as push |
| `work_id` | `jobCreate` | `tracked_work` (durable) | yes, by job id; the dispatch ask's cid is stored on the job |

Delivery to an agent is push. `PeerDelivery` writes a `notify` or `ask` frame
to the peer's WebSocket; the ws-hook puts the full body into the session
(Claude's authenticated inbox, Codex App Server, or an ACP prompt). An ask the
recipient never acks resurfaces on every Stop through `/asks/pending`. A busy
recipient's notify is queued and its ask is deferred to the Stop reminder; an
offline recipient's notify is queued, and an ask to it fails.

So a pull model half-exists: the human inbox is one (durable ids, list, get,
read), and askers can already pull replies by cid. Agent recipients have
neither ids they can name nor a way to fetch.

## 1. Message records

A new table, `peer_messages`, holds one row per (message, agent recipient):
id, kind (`notify`, `ask`, `reply`), recipient peer id and name, sender peer
id and name, correlation id, trace id (the notify's delivery id or the ask's
cid), body, attachments, question, created_at, read_at.

`PeerDelivery` writes the row before choosing a transport, for:

- `Notify` to an agent peer, including the reply notifies that `ack` and
  `decline` send to an asker (kind `reply` when the notify carries a cid);
- `DeliverAsk` to an agent peer, including a delegated delivery.

The human keeps its own inbox; its rows are not duplicated. A broadcast is
recorded once per recipient (kind `broadcast`, #527): each recipient gets its
own frame with its own id, and a pull recipient a nudge. When
the daemon has no state store, nothing is recorded and frames carry no id.
A send that fails outright (no connection, injection refused) deletes its
record, so a recipient never lists a message it was not sent; a queued or
deferred one keeps it.

Frames gain an optional `message_id`. The renderers (ws-hook, Codex bridge,
channel server, queued and pending blocks) add `message-id="..."` to the
`<peer-message>` tag. Old hooks ignore the field.

Rows older than 7 days are deleted lazily, at most once an hour, by the
insert that finds the sweep due. No timer.

## 2. Pull

| Operation | MCP | HTTP | CLI |
| --- | --- | --- | --- |
| list | `list_messages(unread_only?, limit?)` | `GET /messages?peer=&unread=1&limit=` | `repowire peer messages [--unread]` |
| peek | `read_message(message_id, peek=true)` | `GET /messages/{id}` | `repowire peer read ID --peek` |
| fetch | `read_message(message_id)` | `POST /messages/{id}/read` | `repowire peer read ID` |

Over MCP a peer sees only its own messages; the anonymous `mcp-http` identity
has no inbox. The HTTP routes sit behind the daemon's auth like `/inbox`; a
read with `?peer=P` answers only when P is the recipient (the Pi and OpenCode
plugins use this). A fetched body is framed exactly as its push would have
been, including the plain human form when the human sent it, so pulling never
weakens the human/peer trust boundary.

`read_at` means the body reached the recipient: set by a fetch, or by a push
the transport confirmed delivered in full. A queued or deferred push, and
every nudge, leave the message unread until fetched.

## 3. Pull delivery

`message_delivery` is a per-peer setting, `push` (default) or `pull`, kept in
its own table keyed by `peer_id` so it survives a daemon restart (peer
metadata is live-only, so it would silently revert). It is set with
`set_message_delivery(mode)`, `repowire peer delivery push|pull`, or
`POST /peers/{id}/message-delivery`. It mirrors `reply_delivery`, which is the asker's choice for
replies; this one is the recipient's choice for everything addressed to it.

In pull mode the frame text is a nudge instead of the body:

```
[message msg-1a2b3c4d: notify from @reviewer, 1840 chars] First line of the body…
↳ read_message("msg-1a2b3c4d")
```

An ask nudge keeps its correlation id and the ack hint, so the Stop reminder
and `ack` work unchanged. The nudge goes through the same transport choice
(WebSocket, ACP, queue), so pull changes what is pushed, not how. Pull mode
needs a message record; without a store the body is pushed as before.

## 4. Delegation

`delegate(correlation_id, peer_name, note?)`, `POST /asks/{cid}/delegate`,
`repowire peer delegate CID PEER [--note TEXT]`.

Rules, checked before anything changes:

- The ask is open, local (neither end on another machine), and is not the
  dispatch ask of a live job (job completion follows the assigned peer's turn,
  so moving the ask would let the wrong turn complete it).
- The caller is the current recipient. A caller the daemon cannot identify
  (an authenticated HTTP client, anonymous HTTP MCP) may delegate, as it may
  ack today.
- The target resolves, is addressable, and the delegator may reach it under
  the ordinary circle rules. It is neither the asker (decline instead) nor
  the current recipient.
- The chain has fewer than 8 hops.

Effects:

1. The tracker retargets the ask atomically (`ToPeerID`, `ToPeerName`) and
   appends a hop `{from, to, note, at}`. The correlation id is unchanged.
2. The ask is delivered to the new recipient from the delegator, with a first
   line `[delegated by @B for @A: note]`, the original attachments and
   question. It is never sent as the asker: B wrote the note and chose to pass
   the body on, so B's words must not carry A's authority (if A is the human,
   that would frame B's note as a human instruction). Reminders follow
   `ToPeerID`, so they move to the new holder and stop for the old one.
3. If delivery fails (no connection, injection refused, unknown target) the
   hop is undone and the error is returned: the ask stays with the delegator.
   A busy target is not a failure; the ask waits for its Stop reminder, as a
   fresh ask would.
4. The ledger gets a `delegated` stage and the event stream an
   `ask_delegated` event. `/asks/pending` and `wait_on_ack` include the chain.
5. The reply reaches the original asker framed `[ack #cid from @C via @B]`.

**One claim per ask.** The recipient changes, and the holder resolves the
ask, only under the tracker's per-ask claim: bare ack, ack with a body,
decline, answer, a follow-up's `reply_to` close, delegation (checks,
retarget, delivery and rollback together), a runtime's completion, and a
question's timeout default. Checks run on the claimed snapshot, so they stay
true until release. A request that meets a claim is refused with 409 instead
of acting. Work that must not be lost (a runtime's completion, a timeout
default) waits instead: `RunClaimed` queues it and the release starts it, with
no polling. A completion then lands only if its peer still holds the ask, so
one from a peer the ask was delegated away from is dropped, and one that met
a hand-off that failed is not. A holder's result already stashed for an
offline asker pins the ask: it cannot be delegated or resolved again.

**Exactly once.** So a second concurrent resolution, or a delegation racing
it, is refused instead of sending a second reply. A former holder's
ack, decline or answer is refused with 409 naming the current holder. Before
this change two concurrent acks with a body could both deliver: with sends
held open, 16 racing acks delivered 16 replies. The claim makes it one.

## Relation to jobs

Jobs are the durable, heavyweight form of handed-off work: an owner, an
assignee, attempts, progress, a result, and recovery across restarts. An ask
is the lightweight form: in memory, one reply. Delegation gives asks the one
job property they lacked, a change of assignee, while keeping the reply path
the asker already waits on. Delegating a job is a different operation (move
`assigned_peer_id`, open a new attempt) and is left for later; this version
refuses to delegate a job's dispatch ask.

## Compatibility

- No behaviour changes unless a peer opts into pull or someone calls
  `delegate`. Push frames carry the same text plus an optional field.
- Schema: two new tables and an index (migration v18).
- New MCP tools: `list_messages`, `read_message`, `set_message_delivery`,
  `delegate` (also as Pi and OpenCode plugin tools). SessionStart context and
  the Stop reminder mention `delegate` beside `decline`. Codex setup
  pre-approves `delegate`, `list_messages` and `read_message` with the other
  ask-resolution tools.

## Not in this version

Broadcast records and the `daemon.message_delivery` default shipped later
(#527).


Delegation across machines; delegating jobs; a dashboard message browser (the dashboard
shows delegation through the event stream and ask traces).

## Tests

- Store: insert, list (unread, limit, recipient scoping), get, mark read,
  retention sweep.
- Delivery: records for notify, ask, and reply; frames carry `message_id`;
  pull mode pushes a nudge; push mode marks read on delivery.
- Delegation: A asks B, B delegates to C, C acks, A receives exactly one
  reply naming C via B. Chains (B to C to D). Delegating to an offline peer
  is refused and the ask stays with B. A former holder's ack is refused.
  Concurrent acks send one reply. Job asks, cross-machine asks, the asker,
  self, unknown and non-addressable targets are refused.
- MCP and CLI: tool round-trips with caller scoping.
