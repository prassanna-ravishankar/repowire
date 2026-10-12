---
name: feedback_git_pull_before_push
description: Always git pull --rebase before every git push, including feature branches.
metadata:
  type: feedback
---

Run `git pull --rebase` before every `git push`, including feature-branch pushes, not only release pushes to main.

**Why:** Parallel peers (codex reviewers, orchestrators) commit to the same branches and shared checkouts (Prass, 2026-09-28).

**How to apply:** Pushing without pulling risks a non-fast-forward or clobbering a peer's work. Related: [[feedback_check_branch_before_commit]].
