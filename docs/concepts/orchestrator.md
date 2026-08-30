# Orchestrator pattern

An orchestrator is a peer whose job is coordinating other peers. Nothing in the daemon enforces this — `role=orchestrator` is metadata, not access control. It is a workflow: one long-running session you address from your phone or dashboard, which then asks and notifies other peers on your behalf.

Worth setting up when you have more than a few peers and find yourself routing decisions manually. Skip it for two-peer setups.

## The loop

A typical orchestrator runs a loop like:

1. **Scan a board or queue** (GitHub Project, beads, your own todo source).
2. **Dispatch** to the right project peer with `ask(peer, brief)` when the work needs closure, or create a durable job when it needs lifecycle/result state; flip the board item to `In Progress`.
3. **Track** as the peer reports via `ask`/`notify` back. `set_description` on each peer keeps the dashboard honest.
4. **Review** completed work. `review_queue()` surfaces PRs the peer has touched that you still owe a review on; `mark_reviewed(pr_url)` clears them.
5. **Close out** when work lands: update the board, summarize the impact, and clean up peers/worktrees when they are no longer needed.

For independent work, the orchestrator should fan out instead of serializing
everything through one session. Split board items by owner and worktree, dispatch
plan/review lanes in parallel where useful, schedule watchdog wakes for
long-running lanes, and keep the board as the single source of truth.

## Memory and procedures

The orchestrator workspace is a curated procedure layer, not a complete history store. Keep user communication preferences in `comms.md`, active project scope in `projects.md`, durable operational lessons in `memory/*.md`, and reusable procedures in local skills under `.agents/skills/`.

Use markdown memory only for rules that should change future behavior. Detailed recall belongs in session history and, as the v0.13 session-native train lands, SQLite-backed session/timeline search. That split keeps prompt context bounded while preserving the ability to audit past decisions.

This memory is orchestrator-scoped. Other peers do not inherit it implicitly; give them the relevant context through explicit briefs, project-local agent files, native runtime skills, or future session/timeline lookup when that is the right surface.

For inbound asks and notifications targeting a peer registered as
`role=orchestrator`, the daemon performs a small recall pass before delivery.
It scans `comms.md`, `projects.md`, and `memory/*.md` in the orchestrator
workspace and prefixes the delivered message with a bounded `[repowire recall]`
block when the inbound text matches stored preferences or procedures. This is
role-as-data: the daemon reads the peer's existing role field. It is not a
persona declaration or self-attestation ceremony.

Local skills are the on-demand procedure layer. `.agents/skills/` is the canonical cross-runtime location; Claude Code also sees the same skills through `.claude/skills/` symlinks. The top-level orchestrator instructions stay compact and always-loaded. Shipped skills cover general mechanics such as coordination, delegation, durable jobs, agent folders, skill creation, memory, handover, review cycles, worktree isolation, cleanup, and divergent ideation (adhd skill); user- or project-specific preferences belong in local workspace files, not the product template.

## Co-orchestrators

A second orchestrator peer can co-exist as an observer or learner without colliding. Use [`orchestrator_status`](../reference/mcp-tools.md#orchestrator_status) before dispatching long-running work to confirm a live orchestrator is present in the target circle. The call returns presence, name, peer id, last-seen timestamp, and the staleness threshold — *not* a snapshot of mesh state.

Pair runtimes: a `claude-code` orchestrator alongside a `codex`, `opencode`, or `pi` one keeps the mesh moving when one runtime hits credit limits or rate caps.

## Scheduled check-ins

`schedule_create(to_peer, text, fire_at, kind="notify")` defers a single future message — fire-and-forget if `kind="notify"`, or an opened ask thread if `kind="ask"`. `schedule_cron(to_peer, text, cron, kind="notify")` adds recurring check-ins, and `schedule_self(text, fire_at=...|cron=...)` is the convenience form for self-wake reminders.

Scheduled background work should stay quiet by default. Use it for watchdogs,
summaries, cleanup, and review nudges, with a clear owner and cancellation path.
Use `kind="ask"` for scheduled checkpoints or review handoffs that must be
closed; keep `kind="notify"` for reminders that do not gate progress.
Durable board or memory writes should happen only for real state transitions or
forward-applicable lessons, not just because a hook fired.

## Persona (SOUL.md)

The orchestrator can carry a persistent identity through a `SOUL.md` file. The
workspace `AGENTS.md` references a stable `@SOUL.md` shim; `repowire
orchestrator persona use <name>` points that shim at the selected persona file.
Persona guidance sits below user/orchestrator directives and above untrusted
retrieved content. See [personas](./personas.md) for layout, CLI, and
precedence.

## When *not* to orchestrate

- Two peers, ad-hoc work. Talk directly.
- Strict bursts (one push, one CI run). The board overhead isn't worth it.
- Heterogeneous tasks with no shared queue. The orchestrator helps when there's a queue to drain.
