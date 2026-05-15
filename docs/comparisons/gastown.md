# vs Gastown

## What it is

[Gas Town](https://github.com/steveyegge/gastown) is a coding-agent orchestrator released by Steve Yegge on January 1, 2026. A "mayor" agent oversees a "town" of coders, reviewers, and supervisors, all powered by Claude. Work state persists in git-backed hooks. A structured issue tracker called **beads** acts as external memory for the agent fleet — agents reference, update, and share beads to maintain continuity. A mail-style messaging system uses Claude Code hooks in `.claude/settings.json` for mail injection at session start.

Gas Town is built to coordinate 20-30 parallel agents grinding through a backlog.

## Workflow boundary

Gas Town's job is **async fleet orchestration at scale**. Agents work autonomously against a queue (beads), communicate via persistent mail, and the mayor agent supervises. The mental model is a town where workers go about their jobs and the mail system holds the team together.

Repowire's job is **live sync routing for a smaller mesh**. Agents talk to each other in real time, threads have explicit lifecycles (`ask` → `ack`), and control surfaces (dashboard, Telegram, Slack) sit in the same routing layer.

## Architecture difference

- **Gas Town.** Async-first. Mail is persistent; beads are the durable work queue. Agents don't need to be online at the same time. State is git-backed.
- **Repowire.** Sync-first. `ask` opens a thread now; `ack` closes it. Routing requires both peers online (the recipient may not be in-turn at the moment, but their session must exist). State is daemon-resident.

If repowire is a phone call, Gas Town is email plus a project manager.

## When to use repowire instead

- 5-10 peers, not 30. Emergence works at this scale; the overhead of a mail system and a fleet manager doesn't pay off.
- You want live back-and-forth. The agents are *actively* working with you in the loop.
- You're not ready to spend $100/hour on parallel agents. Repowire's local-first model has near-zero idle cost.
- You want a single dashboard + phone view of the live mesh, not a queue of pending mail.

## When to use Gas Town instead

- 20-30 agents grinding through a backlog, with you mostly absent.
- Tasks that can run async over hours or days, where mail-based handoff is fine.
- You want a "mayor" pattern with role specialization (coders, reviewers, supervisors) baked into the system.
- You're comfortable with the credit burn of parallel autonomous fleets.

## Can you use both?

Possibly. Gas Town and repowire both use beads — Gas Town as its native work-tracking surface, repowire as the recommended issue tracker for any project (including the repowire repo itself, per `CLAUDE.md`). A peer in a repowire mesh could query beads the same way a Gas Town worker does.

The transports don't mix cleanly: Gas Town's mail is its own system and repowire's `ask`/`notify` is its own system, with no bridge in either direction in the MVP. If you want both shapes (sync live mesh + async fleet) at once, run them side by side and treat each tool for what it's good at, rather than trying to make one stand in for the other.
