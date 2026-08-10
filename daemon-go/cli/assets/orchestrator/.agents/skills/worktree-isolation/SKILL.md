---
name: worktree-isolation
description: Use when splitting work across peers, creating or selecting feature worktrees, preventing overlapping edits, or deciding whether to reroute implementation out of the main worktree.
---

# Worktree Isolation

Use one worktree per independent implementation concern. This keeps peers from editing the same files blindly and makes review/cleanup tractable.

## When to isolate

Create or select an isolated worktree when:

- Two peers may implement in parallel.
- The change is large enough to outlive the current session.
- The work is speculative, risky, or likely to need review.
- The main worktree has unrelated dirty state.

Use the current worktree for small, direct, low-risk edits when it is clean enough and no other peer owns the same files.

## Before dispatch

Check:

```bash
git status --short --branch
git worktree list
repowire peer list
```

Tell the peer which worktree and branch it owns, what files are likely in scope, and whether it may commit or only report.

## Guardrails

- Do not put two implementation peers on overlapping files in the same worktree.
- Make one lane review-only when overlap is unavoidable.
- Stop or reroute stale peers before reassigning their write scope.
- Never clean up a worktree before checking dirty and unpushed work.
