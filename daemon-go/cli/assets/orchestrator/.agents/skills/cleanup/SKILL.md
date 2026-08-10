---
name: cleanup
description: Use when cleaning up peers, sessions, tmux panes, processes, feature worktrees, branches, generated artifacts, or after a delegated lane or PR is complete.
---

# Cleanup

Cleanup has three parts: mesh registration/session, terminal/process, and workspace artifacts. Treat them separately.

## Before cleanup

Confirm the work is complete, merged or no longer needed, and no peer/reviewer still needs the workspace. Preserve dirty, unmerged, or unpushed user work unless the user explicitly asked for destructive cleanup.

## Safe sequence

1. Check git state in the relevant worktree:
   ```bash
   git fetch origin
   git status --short
   git log @{u}..HEAD
   ```
2. Deregister the peer only when it is no longer needed.
3. Verify terminal state separately:
   ```bash
   tmux list-windows -t <circle>
   ```
4. Kill pane/window only after verifying it is disposable or the user asked.
5. Remove feature worktree and branch only after merge/disposal is clear:
   ```bash
   git worktree remove <feature-worktree-path>
   git worktree prune
   git branch -d <feature-branch>
   ```
6. Re-check mesh, tmux, and worktree state.

## Anti-patterns

- Treating stale mesh rows as proof the terminal is dead.
- Killing terminals that may contain resumable state.
- Pruning worktrees before checking dirty/unpushed work.
- Cleaning up before review or verification is complete.
