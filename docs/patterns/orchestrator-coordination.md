# Orchestrator coordination

When the mesh has more than a few peers, manually routing work between them gets old. An orchestrator peer is the workflow that fixes this: one long-running session that picks up tasks, dispatches them to the right project peer, tracks progress, and reviews work as it lands.

## Setup

Spawn the orchestrator in its own tmux window:

```bash
cd ~/orchestrator && claude
```

In that session, register it as an orchestrator (just metadata):

```text
set role to orchestrator
```

Or simply work in a session named `orchestrator` — by convention other peers will treat any peer with `role=orchestrator` as the dispatch point. The role field shows up in `list_peers`.

## The loop

1. **Scan** the work queue. GitHub Project, beads, a markdown checklist — whatever your team uses.
2. **Dispatch**: `notify_peer(project_a, "claim this task: <brief>")`. Flip the board item to `In Progress`.
3. **Receive** progress via `ask` / `notify` from project peers. `set_description("working on X")` on each peer keeps the dashboard honest.
4. **Review** completed work. `review_queue()` surfaces PRs you owe a look. `mark_reviewed(pr_url)` clears them after the pass.
5. **Release** when a batch lands. Tag, push, notify the team channel.

## Before dispatching

Call `orchestrator_status(circle="...")` first to confirm a live orchestrator (you) is present in the target circle. Returns `present, peer_name, peer_id, last_seen, stale_after_seconds`. This is a presence check, not a mesh snapshot.

Useful when you have two orchestrators (one Claude Code, one Codex; an observer alongside a driver) and want to avoid double-claiming work.

## Co-orchestrators

A second orchestrator can run in parallel as an observer or learner. Use `orchestrator_status` to coordinate. Common shape:

- **Driver** (claude-code) — dispatches and reviews.
- **Observer** (codex or gemini) — watches the mesh log, runs the same review pass after the driver, surfaces findings the driver missed.

Pairing runtimes also hedges against rate limits and credit caps on either side.

## Scheduled check-ins

`schedule_create(to_peer, text, fire_at, kind="notify")` defers a single future message. Use `schedule_cron(to_peer, text, cron, kind="notify")` for recurring check-ins, or `schedule_self(text, fire_at=...|cron=...)` when the orchestrator is scheduling its own wake-up.

Typical uses:

- Wake yourself in 25 minutes to check on a long-running migration peer.
- Nudge a peer for a status update at the top of the hour, once or on a cron cadence.
- Schedule a release tag right after the freeze window ends.

## When to skip the orchestrator

- Two peers, ad-hoc work. Talk directly.
- One-off bursts. The board overhead isn't worth it.
- Heterogeneous tasks with no shared queue. The orchestrator helps when there's a queue to drain.

## See also

- [Concepts: orchestrator pattern](../concepts/orchestrator.md) — same idea, less how-to.
- [`orchestrator_status`](../reference/mcp-tools.md#orchestrator_status), [`review_queue`](../reference/mcp-tools.md#review_queue), [`schedule_create`](../reference/mcp-tools.md#schedule_create), [`schedule_cron`](../reference/mcp-tools.md#schedule_cron).
