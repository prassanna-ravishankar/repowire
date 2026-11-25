# Wire actions: reassign, nudge, cancel

Status: implemented. Step 1 (no way back, generations), step 2 (operator actions over HTTP and the CLI) and step 3 (provenance and `peer_wire_visibility`, see `wire-observability.md` "Provenance as built") on 2026-11-24; step 4 (the orchestrator MCP tools, see "Orchestrator authority as built") on 2026-11-25. The intervention slice of
`wire-observability.md` (section 4), made concrete. It builds on the
read-only wire view (`service.Wire`) and on `delegate`
(`hub/routes_ask_delegate.go`, `AskTracker.Delegate`).

## Where the actions plug in

- **Finding the thread.** An action takes a thread id as `wire_query`
  shows it. `Wire.Resolve` maps it (an `h-` alias included) to the
  correlation id, and `Wire.Thread` gives the state the checks need:
  open or closed, holder, `via`, `protected`, whether a reply is stashed.
- **Changing ownership.** The tracker stays the only owner of an ask's
  holder. Reassign and cancel are tracker operations beside `Delegate`, and
  they run the way delegate does: under the ask's `Claim`, so no reply,
  answer, completion or other hand-off can interleave, with delivery inside
  the claim and rollback on failure.
- **Recording.** Each action writes a trace stage (so the wire view shows
  it with no new store) and an event naming the actor.

## Common rules

Refused for every action: a closed ask, an ask with a remote end, a job's
dispatch ask (use the job tools), and an ask whose reply is stashed for an
offline asker (the stash's redelivery is not claimed, so acting on it could
produce both a reply and a cancellation; delegate refuses this already).

Two actors:

- **Operator** (HTTP and CLI) may act on any local ask, protected or not,
  since it can already read everything. The event records
  `actor: operator`, because the token is not an identity. Operator proof
  for these mutations is the daemon token, required, with no other way in:
  the routes do not use `requireAuth`, which also admits every request on a
  tokenless daemon and any loopback request carrying `Sec-Fetch-Site:
  same-origin` or a matching `Referer`. A local agent can forge those
  headers with curl. A daemon without a token refuses the routes.
  This is as strong as an HTTP credential can be on a machine where agents
  run as the same user: an agent that can read `~/.repowire/config.yaml`
  holds the token. So the protected-thread and circle rules bind agents on
  the MCP path, and the operator routes are only as private as that file
  (#581). The real human proof, for actions that must be beyond an agent's
  reach, is a confirmation through the human channel: the human inbox
  already owns proof that a human answered, and a Telegram reply cannot be
  produced by a local agent. That is the socket such actions plug into
  later.
- **Orchestrator** (MCP, proven identity, `role=orchestrator`, checked per
  call) is refused on protected threads, and every party the ask ever had
  (asker, each holder, each delegator) and the destination must be in the
  circle it holds the role for. That needs the provenance recorded at
  creation and at each hop (`wire-observability.md` section 2). Until it
  exists every thread is protected, so agents cannot act and only the
  operator can.

## Prerequisite: no way back, and assignment generations

An agent's resolution (ack, decline, answer) names only the correlation id,
so the daemon matches it to the holder by peer id. That is sound only if an
ask never returns to a peer that held it before: after B to C and back to B,
B's stale reply would look current, and nothing in the tool call could tell
them apart.

- **No way back.** Delegate and reassign refuse a destination that held the
  ask before (`Ask.HeldBefore`), as they already refuse the asker. A former
  holder's replies are refused anyway, so moving the ask back to it would
  contradict that rule. Peer-id matching then stays sound for agent
  resolutions.
- **Generations for the daemon's own callbacks.** Each hand-off that stands
  increments `Ask.Generation` and stamps it on the hop; `UndoDelegation`
  restores it. A callback the daemon registered when it delivered an
  assignment carries that generation, and an older one is dropped: an ACP
  completion, and any delivery still queued or in flight to the old holder.
  (A question's timeout default is the asker's, not the holder's, so it
  needs none.)

## Reassign

`reassign(thread_id, to, reason)`

A thread need not be stuck: an orchestrator may move an ask because another
peer fits it better. The reason is required, short (at most 200
characters), and recorded on the thread.

- **Ownership.** The holder moves from H to N. The correlation id, the
  asker, the reply path and the reply mode stay as they are. The hop is
  recorded as `{from: H, to: N, by: actor, reason}`, so attribution survives:
  H did not hand it off; the actor moved it. The generation increments, and
  any later ack, decline or answer from H is refused, as for a former
  delegator. A tracker operation `Reassign(cid, by, to, reason)` shares
  `Delegate`'s mutation and `UndoDelegation`'s rollback, without
  delegate's "the caller must be the holder" rule. It is not `Delegate`
  with that check cleared, since that would erase who acted. `Delegation`
  gains `By` and `Reason`. A reassign counts toward `MaxDelegations`, and it
  keeps delegate's refusals: a stashed result (`ErrResultWaiting`), a
  quiesced destination (`ErrQuiesced`), the asker, and any former holder.
- **H's pending copies go.** Under the claim, before delivering to N, the
  ask's ACP prompt in flight to H and any queued delivery for H with this
  correlation id are dropped, the queued rows recorded `dropped` with
  reason `reassigned`. Otherwise H could still receive the ask after it
  moved.
- **The new holder N** receives the ask from the actor, never as the asker:
  `[reassigned by @orch from @H for @asker: reason]`, followed by the ask as
  delegate frames it. That passes the body on, which is why protected
  threads are refused for agents.
- **The former holder H** gets a notify with no body: ask `#cid` moved to
  @N by @orch, and a reply from H will be refused.
- **The asker** gets one line: `your ask #cid moved from @H to @N by @orch:
  reason`. The asker owns the ask, so it should not first learn of the move
  from an unexpected reply. N's reply then arrives through the usual reply
  path, attributed to N with the hop chain, as after a delegation.
- **On a failed delivery to N**, the hop is undone under the claim, H holds
  the ask again, and the action fails loudly. Neither H nor the asker is
  told, because nothing changed: both notices go out only after N accepted
  the ask.
- **Recorded:** a `reassigned` stage (peer N, detail `by`, `from`, and the
  reason, which is agent-written text and so counts as a body, not
  envelope, in the wire view) and an `ask_reassigned` event. The wire reducer treats `reassigned` as a hop,
  like `delegated`.

## Nudge

`nudge(thread_id, note?)`

- **Ownership.** None changes.
- **The holder** gets a notify: `[nudge from @orch: ask #cid from @asker,
  open 42m: note]`. The note is the actor's; the ask's body is not
  repeated.
- **Nobody else** is told.
- **Why a tool rather than `notify_peer`.** The draft said `notify_peer`
  already nudges. A thin tool adds what that cannot: a `nudged` stage on the
  ask's own trace (so the timeline shows it, and the next reader sees
  "nudged 5m ago" instead of nudging again), and a limit of one nudge per
  thread per 10 minutes. It sends through the ordinary notify path.

## Cancel

`cancel_ask(thread_id, reason)`

- **Ownership.** The ask closes. Cancel reuses the resolution path
  (`deliverAskResolution`, `AskTracker.Resolve` with reason `cancelled`, the
  actor and the reason), so it gets the commit, the stash for an offline
  asker and `wait_on_ack` for free: `wait_on_ack` and pull readers see the
  committed outcome, and the push to an offline asker uses the reply path's
  stash and redelivery. The promise is one committed outcome, not one
  delivered frame. Every `close_reason` consumer (the hooks, the channel
  server, the bridges, `repowire why`) must accept the new value
  `cancelled`; the implementation checks each.
- **The holder's pending copies go**, as for reassign (reason
  `cancelled`).
- **The asker** receives the outcome on its reply path:
  `[ask #cid cancelled by @orch: reason]`, and `wait_on_ack` returns
  `cancelled`.
- **The holder** gets a notify: ask `#cid` was cancelled by @orch, stop
  work on it, and a reply will be refused. Cancel ends the protocol thread;
  it cannot stop work the holder's runtime has already started.
- **Former holders** are not told.
- **Recorded:** the `closed` stage with `close_reason: cancelled` and
  detail `by`, and an `ask_cancelled` event.

## Surfaces

MCP tools `reassign`, `nudge`, `cancel_ask` for the orchestrator; HTTP
`POST /wire/threads/{id}/reassign|nudge|cancel` and
`repowire wire reassign|nudge|cancel` for the operator. `wire_show` prints
each available action as a serialized call, never as text spliced from
fields.

## Order of work

1. No way back to a former holder, and assignment generations (a tracker
   change with its own tests).
2. The three actions for the operator only (HTTP and CLI), behind the
   required token. Prass can steer stuck work right away, without waiting
   for provenance.
3. Provenance on `created`, `delegated` and `reassigned` stages, and
   `peer_wire_visibility`.
4. The MCP tools for orchestrators, behind the protected-thread and circle
   rules.

## Orchestrator authority as built

- **One path for both actors.** The HTTP routes and the MCP tools both call
  `wireAction` (`hub/ask_actions.go`) with an actor: the operator, or the
  proven orchestrator. The actor decides the sender, the attribution
  (`by` on the hop, trace and event is the orchestrator's `peer_id`) and
  the notices' wording; nothing else differs.
- **The rule lives in the wire view.** `service.Wire.OrchestratorMay`
  decides from the thread as reduced: an open ask, not protected, every
  recorded party in the orchestrator's circle, and for a reassign a
  destination that is in that circle now, not human or service, and not
  private. A circle of `""` counts as `default`. `AuthorizeOrchestrator`
  runs it on the thread as recorded at the moment of the action, under
  the ask's claim, so a concurrent delegate cannot add a party between the
  check and the move.
- **The acting orchestrator is a recorded party.** A reassign's stage
  records the new holder and the orchestrator, so the thread carries the
  circle it passed through. The operator is not a peer and records
  nothing.
- **`wire_show`** ends with an `action`/`arguments` table, each row the
  tool name and its arguments encoded as JSON, only when `OrchestratorMay`
  admits the caller.
- **Not done:** keying visibility by a durable identity rather than
  `peer_id` (a resumed session under a new `peer_id` sets it again), and a
  version on the recorded provenance. Neither changes a decision today.

## Decisions and open questions

- Decided (orchestrator, 2026-11-24, under Prass's autonomy grant): a
  reassign does not require the thread to be stuck, and it requires a short
  reason, recorded on the thread.
- Decided (same): the asker is told on reassign, in one line.
- Decided (same, after review): an ask never returns to a former holder,
  so agent resolutions stay matched by peer id and generations guard only
  the daemon's own callbacks.
- Settled in review: one nudge per 10 minutes per thread.
