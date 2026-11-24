# Wire actions: reassign, nudge, cancel

Status: design, not built (2026-11-24). The intervention slice of
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

- **Operator** (daemon token, HTTP and CLI) may act on any local ask,
  protected or not, since it can already read everything. The event
  records `actor: operator`, because the token is not an identity.
- **Orchestrator** (MCP, proven identity, `role=orchestrator`, checked per
  call) is refused on protected threads, and every party the ask ever had
  (asker, each holder, each delegator) and the destination must be in the
  circle it holds the role for. That needs the provenance recorded at
  creation and at each hop (`wire-observability.md` section 2). Until it
  exists every thread is protected, so agents cannot act and only the
  operator can.

## Prerequisite: assignment generations

Today a resolution is matched to the holder by peer id. After B to C and
back to B, B's first, stale completion looks current. Each successful hop
(delegate or reassign) increments `Ask.Generation`. Every resolution and
runtime callback (ack, decline, answer, ACP completion, a question's timeout
default) carries the generation it was issued under, and an older one is
refused or dropped. Reassign depends on this: it is what makes the former
holder's late work harmless.

## Reassign

`reassign(thread_id, to, note?)`

- **Ownership.** The holder moves from H to N. The correlation id, the
  asker, the reply path and the reply mode stay as they are. The hop is
  recorded as `{from: H, to: N, by: actor, note}`, so attribution survives:
  H did not hand it off; the actor moved it. The generation increments, and
  any later ack, decline or answer from H is refused, as for a former
  delegator. A tracker operation `Reassign(cid, by, to, note)` shares
  `Delegate`'s mutation and `UndoDelegation`'s rollback, without
  delegate's "the caller must be the holder" rule. It is not `Delegate`
  with that check cleared, since that would erase who acted.
- **The new holder N** receives the ask from the actor, never as the asker:
  `[reassigned by @orch from @H for @asker: note]`, followed by the ask as
  delegate frames it. That passes the body on, which is why protected
  threads are refused for agents.
- **The former holder H** gets a notify with no body: ask `#cid` moved to
  @N by @orch, and a reply from H will be refused.
- **The asker** is not messaged, so a routine reassign makes no noise. Its
  `wait_on_ack`, its pending view and the wire view show the new holder, and
  N's reply arrives through the usual reply path, attributed to N with the
  hop chain, as after a delegation.
- **On a failed delivery to N**, the hop is undone under the claim, H holds
  the ask again, and the action fails loudly. Neither H nor the asker is
  told, because nothing changed.
- **Recorded:** a `reassigned` stage (peer N, detail `by`, `from`) and an
  `ask_reassigned` event. The wire reducer treats `reassigned` as a hop,
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

- **Ownership.** The ask closes. Under the claim, the tracker commits a
  result: close reason `cancelled`, the actor, and the reason. Committing
  is separate from delivering it, so `wait_on_ack` and pull readers see the
  committed outcome, and the push to an offline asker uses the reply path's
  stash and redelivery. The promise is one committed outcome, not one
  delivered frame.
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

1. Assignment generations (a tracker change with its own tests).
2. The three actions for the operator only (HTTP and CLI). Prass can steer
   stuck work right away, without waiting for provenance.
3. Provenance on `created`, `delegated` and `reassigned` stages, and
   `peer_wire_visibility`.
4. The MCP tools for orchestrators, behind the protected-thread and circle
   rules.

## Open questions

- Should reassign require the thread to be stuck first? This note allows it
  on any open, local thread that is not refused above, relying on
  generations to make the former holder's late work harmless.
- Should the asker be told on reassign? This note says no, keeping it
  visible in the wire view and in the reply's attribution instead.
- Is one nudge per 10 minutes per thread the right limit?
