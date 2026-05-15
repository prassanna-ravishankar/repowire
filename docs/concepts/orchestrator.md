# Orchestrator pattern

An orchestrator is a peer whose job is coordinating other peers. Nothing in the daemon enforces this — `role=orchestrator` is metadata, not access control. It is a workflow: one long-running session you address from your phone or dashboard, which then asks and notifies other peers on your behalf.

Worth setting up when you have more than a few peers and find yourself routing decisions manually. Skip it for two-peer setups.

## The loop

A typical orchestrator runs a loop like:

1. **Scan a board or queue** (GitHub Project, beads, your own todo source).
2. **Dispatch** to the right project peer with `notify_peer(peer, brief)`; flip the board item to `In Progress`.
3. **Track** as the peer reports via `ask`/`notify` back. `set_description` on each peer keeps the dashboard honest.
4. **Review** completed work. `review_queue()` surfaces PRs the peer has touched that you still owe a review on; `mark_reviewed(pr_url)` clears them.
5. **Release** when a batch lands. Tag, push, notify.

## Co-orchestrators

A second orchestrator peer can co-exist as an observer or learner without colliding. Use [`orchestrator_status`](../reference/mcp-tools.md#orchestrator_status) before dispatching long-running work to confirm a live orchestrator is present in the target circle. The call returns presence, name, peer id, last-seen timestamp, and the staleness threshold — *not* a snapshot of mesh state.

Pair runtimes: a `claude-code` orchestrator alongside a `codex` or `gemini` one keeps the mesh moving when one runtime hits credit limits or rate caps.

## Scheduled check-ins

`schedule_create(to_peer, text, fire_at, kind="notify")` defers a single future message — fire-and-forget if `kind="notify"`, or an opened ask thread if `kind="ask"`. Recurring schedules are not supported in the MVP; if you need a cadence, your orchestrator should re-schedule the next one when it handles the current one.

## When *not* to orchestrate

- Two peers, ad-hoc work. Talk directly.
- Strict bursts (one push, one CI run). The board overhead isn't worth it.
- Heterogeneous tasks with no shared queue. The orchestrator helps when there's a queue to drain.
