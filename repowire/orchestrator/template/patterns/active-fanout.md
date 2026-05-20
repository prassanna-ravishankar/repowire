---
name: active-fanout
description: Default orchestrator shape for parallelizing independent board lanes.
triggers: [board-triage, multi-lane-work, project-2, watchdog]
risk: medium
surfaces: [mcp, scheduler, dashboard, telegram]
---

# Pattern: active fan-out

Use this when the board contains multiple independent items or when one item naturally splits into disjoint worktrees, owners, or review lanes. The orchestrator coordinates the shape; peers do the implementation.

## Default shape

1. **Triage the board item.**
   - If it is user-facing product work, keep it on the product tracker for the repo.
   - If it is cross-repo orchestration work, keep it on the orchestrator board.
   - If it is a transient status note, do not create a board item; use session history or a short notify.

2. **Split by ownership.**
   - One worktree per concern.
   - One peer owner per implementation lane.
   - Separate plan/review lanes when the plan can be critiqued independently.
   - Avoid overlapping write scopes. If two peers would edit the same files, make one a reviewer instead of a parallel implementer.

3. **Brief each lane.**
   - Include board item title, repository/worktree, success criteria, docs impact, and reporting surface.
   - Include relevant memory or pattern refs only when they materially change the work.
   - Ask for plan-before-code when a wrong approach would cost more than about 30 minutes of rework.

4. **Set watchdogs.**
   - For each fan-out, schedule a self-wake or peer check-in before the work can go stale.
   - Use `notify` for nudges and routine wakes.
   - Use `ask` only when the future wake must create a tracked thread that requires closure.

5. **Keep one source of truth.**
   - The board owns item state.
   - Peer descriptions expose current focus, not durable history.
   - Session timelines and reports hold detail.
   - Memory stores only forward-applicable lessons: "next time X comes up, do Y differently."

6. **Consolidate without churn.**
   - Update the board at meaningful state changes: Todo -> In Progress -> Done, with explicit blocker notes when needed.
   - Do not write memory or board updates just because a turn ended.
   - Prefer a single end-of-lane summary over many small status edits.

## Productized runtime-hook direction

These are orchestrator-scoped defaults for future automation, not global peer behavior:

- **Input triage hook:** classify each user message as immediate reply, board candidate, memory candidate, dispatch command, or schedule request. Default to no write; ask before creating durable state unless the user explicitly named a board/memory action.
- **Stop consolidation hook:** after orchestrator turns, suggest or stage memory/board/session-summary updates only when there is a durable lesson, a board state transition, or a delegated lane changed status.
- **Scheduled background child peers:** use schedules to wake the orchestrator or a specific child peer for status, summary, cleanup, or review. Every recurring job needs an owner, cancellation path, and quiet default.
- **Automatic description/summary jobs:** keep peer descriptions short and current; write longer summaries to session timeline/report surfaces, not memory.

## Anti-patterns

- **Orchestrator implementation drift.** If the work is substantive product implementation, spawn or reuse a project peer.
- **Serial bottlenecking.** Do not wait for one independent lane before dispatching the next.
- **Noisy durable writes.** A memory file, board edit, or project note must earn its place.
- **Unowned background jobs.** Every schedule needs a clear reason, recipient, and cleanup path.
