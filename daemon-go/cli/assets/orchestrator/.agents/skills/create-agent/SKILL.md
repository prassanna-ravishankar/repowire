---
name: create-agent
description: Use when creating, updating, or explaining a standing Repowire agent folder, worker folder, durable-job executor context, or reusable agent-specific AGENTS.md guidance.
---

# Create Agent

Use an agent folder when a repeatable worker needs standing context: domain rules, tool expectations, privacy boundaries, output format, or durable-job instructions.

Agent folders are a convention, not a registry. Jobs and spawns still target `--path --backend [--profile]`.

## Create

```bash
repowire agents create <name> --backend <runtime>
```

The command creates `.repowire/agents/<name>/AGENTS.md`. For Claude Code it also creates `CLAUDE.md` as a shim. Other supported runtimes load `AGENTS.md` directly.

## What to put in AGENTS.md

Keep it specific to the worker:

- Mission and success criteria.
- Tools or integrations the worker may use.
- Privacy, safety, and escalation boundaries.
- Result format and lifecycle expectations.
- Examples only when they prevent likely ambiguity.

Do not store secrets, personal tokens, absolute machine paths, or one user's private service assumptions in a shipped template.

## Pair with durable jobs

Use the folder as the durable execution context:

```bash
repowire jobs create "Daily brief" \
  --path "$(pwd)/.repowire/agents/daily-brief" \
  --backend codex \
  --cron "0 8 * * *" \
  --prompt "Prepare the brief and update this job with the result."
```

If the worker is only a one-off prompt with no standing context, create a normal job instead of an agent folder.
