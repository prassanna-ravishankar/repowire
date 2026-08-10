---
name: durable-jobs
description: Use when the user asks for durable or recurring background work, work that should survive peer death, retry/cancel/result tracking, spawned-on-demand workers, agent folders, daily briefs, or deciding between repowire jobs and schedule.
---

# Durable Jobs

Use `repowire jobs` when work needs durable lifecycle state, recovery after peer death, retry/cancel/result inspection, spawned-on-demand execution, or recurrence. Use `schedule` only for future ask/notify wakeups to an existing target.

Agent folders are a convention, not a registry. Scaffold them with:

```bash
repowire agents create <name> --backend <runtime>
```

Then create jobs targeting the folder's absolute path with `--path <abs-path> --backend <runtime>`. The folder's `AGENTS.md` is the source of truth; `CLAUDE.md` is only a shim for Claude Code. Other supported runtimes load `AGENTS.md` directly.

`--result-surface` is metadata only until delivery routing exists. Do not claim jobs send Telegram, email, or dashboard notifications automatically. Workers must update job results explicitly.

## Daily email brief

User intent: "Every morning, summarize important email and send me a brief."

Route:

```bash
repowire agents create daily-email-brief --backend codex
repowire jobs create "Daily email brief" \
  --path "$(pwd)/.repowire/agents/daily-email-brief" \
  --backend codex \
  --cron "0 8 * * *" \
  --prompt "Prepare today's email brief. Use the job_id and attempt_id from this prompt when updating lifecycle state." \
  --result-surface telegram
```

Put standing worker guidance in `.repowire/agents/daily-email-brief/AGENTS.md`: email tool expectations, privacy boundaries, what counts as important, and output format. Keep credentials outside the folder and outside job records.

## One-time durable task

```bash
repowire jobs create "Inspect billing webhook compatibility" \
  --path /path/to/billing-repo \
  --backend codex \
  --prompt "Inspect webhook compatibility risk and update this job with a concise result."
repowire jobs run <job_id>
```

Use a job instead of `notify_peer` when the result must survive peer death, be retried, or be inspected later with `repowire jobs result`.

## Wake-up reminder

```bash
repowire schedule self 30m "Check whether the release peer replied."
```

Use `schedule`, not `jobs`, when this is just a future message to an existing session.
