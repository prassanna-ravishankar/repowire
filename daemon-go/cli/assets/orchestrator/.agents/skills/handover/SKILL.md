---
name: handover
description: Use when transferring orchestrator responsibility, summarizing current state for a new peer, recovering after a closed session, or preparing takeover notes.
---

# Handover

Use handover when continuity matters more than chat transcript detail.

## Write a handover

Capture:

- Current objective and why it matters.
- Active lanes, owners, worktrees, and ask/job IDs.
- What is done, what is blocked, and the next decision.
- Verification already run and verification still needed.
- Anything the next peer must not touch.

Prefer links to durable surfaces over pasted logs: job IDs, ask IDs, branch names, issue IDs, and file paths.

## Read a handover

Before acting, verify live state:

```bash
repowire peer list
repowire jobs list
git status --short --branch
```

Assume handover notes can be stale. Reconcile them against the repo, jobs, and active peers before dispatching new work.

## Avoid

- Treating handover as memory.
- Copying entire transcripts.
- Continuing stale implementation after the user has rerouted work.
