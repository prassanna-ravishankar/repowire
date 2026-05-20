# Contributing

See [`CONTRIBUTING.md`](https://github.com/prassanna-ravishankar/repowire/blob/main/CONTRIBUTING.md) in the repository for the contribution workflow, code-quality gates, and release process.

Before opening a PR from a checkout, run the advisory hygiene checklist:

```bash
python3 scripts/pre_pr_hygiene.py
```

The checklist is not a mandatory hook. It points contributors at README, reference docs, mirrored
web docs, `CLAUDE.md` / `AGENTS.md`, and graphify reminders when the changed paths suggest a
product-surface or architecture update.
