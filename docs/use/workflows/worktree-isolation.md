# Worktree isolation

Spawn peers on git worktrees so each one works on a separate branch in isolation. Useful when you want two or three agents writing parallel features without stepping on each other's working tree.

## Why worktrees

A worktree is a separate checkout of the same repo on a different branch, sharing the same `.git` directory. Each agent gets its own `working tree`, so two agents can stage and commit independently without conflicting. When the work lands, the worktree is removed; only the branch remains.

This is different from cloning the repo twice. Worktrees share objects, save disk, and let `git` operations cross-reference the branches naturally.

## Setup

```bash
git worktree add ../project-feat-a feat/a
git worktree add ../project-feat-b feat/b
```

Two new directories, one branch each. Now open one agent in each:

```bash
cd ../project-feat-a && claude
cd ../project-feat-b && codex
```

Both register as peers (different display names, separate paths). Address them by name.

## The pattern

1. Plan the split. Each worktree is a peer; each peer owns one branch.
2. Dispatch work with `ask` when you need an explicit closeout, or create a job
   when you need durable lifecycle/result state. Keep `notify_peer` for FYIs and
   nudges that do not gate progress.
3. Each peer commits and pushes its own branch. No merge conflicts during development; conflicts happen at PR-merge time, not in the working tree.
4. Spawn a reviewer peer (or use an existing one) to review each PR.
5. Merge in dependency order.

## Pairing with the orchestrator

Orchestrators commonly spawn peers on worktrees. The flow:

```python
spawn_peer(
    path="/home/me/projects/project-feat-c",
    backend="claude-code",
    circle="features",
    message="implement feat/c per spec at docs/spec-c.md, commit as you go",
)
```

The spawned peer self-registers within seconds. The orchestrator then `ask`s it
for progress and reviews the PR when it lands.

## Cleanup

```bash
git worktree remove ../project-feat-a
git branch -D feat/a   # if you don't want the branch either
```

Or use the [`worktrunk` skill](https://github.com/AtomBeings/worktrunk) if you want commit-message generation and hook automation alongside the worktree lifecycle.

## When this is too much

- One-off changes that fit in a single branch. Worktrees add ceremony.
- Tasks where the agents need to see each other's commits as they happen. Worktrees deliberately separate working trees; cross-branch reads need explicit `git fetch`.
- Disk-constrained environments. Each worktree still has a full working tree on disk, even though `.git` is shared.

## See also

- [`spawn_peer`](../../reference/mcp-tools.md#spawn_peer) and the spawn runtime profiles in [configuration](../../reference/configuration.md).
- [Orchestrator coordination](orchestrator-coordination.md) for the dispatch side.
