# Pattern: post-merge cleanup

After a PR merges and the work is complete: clean up the registered peer/session, verify the terminal/process is actually gone, prune the worktree/branch/artifacts, and update local main.

## When to reach for it

- A PR you dispatched merged successfully
- The peer that did the work is no longer needed (no continuation planned)
- Worktree was a per-feature `<project>.<feature>` clone, not the main project worktree

## When NOT to reach for it

- The peer has follow-up work queued (continuation turns)
- The worktree is the main project worktree, not a feature clone
- A parallel review is still verifying the merge — don't clean up until they ✅

## Shape

1. **Confirm merge state.**
   ```bash
   git fetch origin
   git log origin/main..HEAD  # should be empty on the feature branch
   git status --short
   ```
   If the branch has unpushed commits, unmerged work, or dirty user changes beyond what was merged, stop — preserve it and investigate before destroying anything.

2. **Update the project's main worktree.**
   ```bash
   cd <project-main-worktree>
   git pull --rebase origin main
   ```

3. **Clear the registered peer/session in the feature worktree.**
   ```python
   mcp__repowire__kill_peer(name="<project>.<feature>-<runtime>")
   ```

4. **VERIFY the terminal/process is actually gone.** A registry cleanup may not kill the underlying tmux window, pane, shell, or worker process. Check:
   ```bash
   tmux list-windows -t <circle>
   ```
   If the window is still there:
   ```bash
   tmux kill-window -t <circle>:<window-name>
   ```
   Or by stable pane id (preferred when available):
   ```bash
   tmux kill-pane -t %<pane-id>
   ```

5. **Prune the worktree.**
   ```bash
   cd <project-main-worktree>
   git worktree remove <feature-worktree-path>
   git worktree prune
   git branch -d <feature-branch>  # local cleanup; -D if not merged-by-name
   ```

6. **Sanity-check the cleanup triad.**
   ```bash
   git worktree list   # feature worktree should be gone
   tmux list-windows -t <circle>   # window/pane gone
   mcp__repowire__list_peers()   # peer gone from mesh
   ```

## Anti-patterns

- **Skipping the tmux verify step.** `kill_peer` lies about pane death. Orphan tmux windows accumulate; eats memory; confuses later audits.
- **Pruning the worktree before confirming the pane/process is dead.** Orphan agents can keep running in a deleted directory. Harmless functionally but pollutes state.
- **Cleaning up before a parallel review is done.** They need the worktree to verify against. Wait for ✅.

## When something looks wrong

- **Branch has unpushed commits, dirty files, or unmerged work not on main:** check `git log @{u}..HEAD`, `git status --short`, and `git ls-remote origin <branch>`. Could be abandoned work or user work. Surface before destroying.
- **`kill_peer` returns 404:** peer was already deregistered (likely from a SessionEnd hook). Tmux pane may still be live; check anyway.
