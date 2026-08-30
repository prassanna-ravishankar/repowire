# Cross-agent review

Have a second agent review the first agent's work before you do. Especially useful across runtimes — a Claude Code session writing a feature, a Codex session reviewing it for security or simplification.

## The flow

1. **Builder peer** finishes a chunk of work and commits or stages it.
2. Builder calls `ask("reviewer", "review the diff on feat/foo for the auth path")`.
3. **Reviewer peer** reads the diff (`git diff`, `gh pr diff`), runs whatever checks it cares about, and acks back with findings.
4. Builder addresses the findings in a follow-up turn.

## Why two agents

The second agent has no commitment to the implementation. It hasn't spent five turns convincing itself the current approach is fine. That neutrality is the value — not raw capability difference.

Pair runtimes for an additional check:

- **Claude Code + Codex** — different model lineages catch different things.
- **OpenCode + Pi** — native plugin and extension paths provide a useful cross-runtime check.
- **Same runtime, fresh session** — works too; the new session loads the diff cold.

## Concrete prompts

For the builder:

```text
ask("reviewer", "I just pushed feat/auth-rework. Review the diff at
  https://github.com/me/repo/pull/42 with a security pass:
  authentication, authorization, session handling. Skim everything else.")
```

For the reviewer (no special prompt needed — repowire-aware agents will run the review on receipt). Tell the reviewer what to focus on, not just "review this."

## Mark it reviewed

The reviewer can call `mark_reviewed(pr_url)` after the pass. The PR drops off the reviewer's `review_queue` at the recorded SHA. If the builder pushes new commits later, the PR resurfaces as `re-review-suggested`.

## See also

- [Orchestrator coordination](orchestrator-coordination.md) for review at fleet scale.
- [`mark_reviewed`](../../reference/mcp-tools.md#mark_reviewed) and [`review_queue`](../../reference/mcp-tools.md#review_queue) reference.
