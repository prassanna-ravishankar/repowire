# Contributing

See [`CONTRIBUTING.md`](https://github.com/prassanna-ravishankar/repowire/blob/main/CONTRIBUTING.md) in the repository for the contribution workflow, code-quality gates, and release process.

Before opening a PR from a checkout, run the advisory hygiene checklist:

```bash
scripts/pre-pr-hygiene.sh
```

The checklist is not a mandatory hook. It points contributors at README, reference docs, mirrored
web docs, and `CLAUDE.md` / `AGENTS.md` when the changed paths suggest a product-surface or
architecture update. It also flags Beads JSONL ledger churn; use
`--restore-beads-ledgers` to back up and restore local-only ledger diffs before opening a PR.
