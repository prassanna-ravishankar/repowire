---
name: review-cycle
description: Use when coordinating PR review, CI triage, second-peer critique, requested-changes loops, merge readiness, or deciding whether implementation needs independent verification.
---

# Review Cycle

Use review cycles when the cost of a missed bug is higher than the cost of an independent pass.

## Loop

1. Define what is being reviewed: branch, PR, ask ID, job ID, or file set.
2. Ask a reviewer peer for findings first, ordered by severity with file/line refs.
3. Route actionable findings to the implementation owner.
4. Verify fixes with tests, CI, and the real behavior when applicable.
5. Summarize residual risk and the merge/release decision separately.

## CI triage

For failed checks, inspect the failing job and logs before guessing. Prefer the user's existing GitHub tooling or plugin when available.

Do not treat green CI as proof of product behavior. UI, daemon workflow, transport, and docs changes often need a targeted smoke test.

## Merge readiness

Call out:

- Tests and checks run.
- Docs impact handled or explicitly deferred.
- Review findings resolved or accepted.
- Whether release/tagging is authorized by the user or project policy.

Do not ship personal release cadence or authority rules in this skill.
