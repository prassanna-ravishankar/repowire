# Jobs and Schedules

Schedules deliver future mesh messages. Jobs represent durable tracked work that can be created, inspected, updated, completed, or canceled through CLI and MCP surfaces.

## Schedules

Use schedules for reminders, future asks, and recurring check-ins:

```bash
repowire schedule self 10m "check CI"
repowire schedule cron orchestrator "@daily" "review open jobs"
```

Scheduling is message-oriented: a delivery can be a fire-and-forget notification or a tracked ask that requires an `ack`.

## Jobs

Jobs are durable work records. They are useful when an orchestrator or human needs to track status, result, cancellation, and recurring worker templates across turns.

```bash
repowire jobs create "Daily brief" --path .repowire/agents/daily-brief --backend codex --cron "@daily" --prompt "Prepare the brief."
```

The MCP surface is `job_create(..., path=..., backend=..., cron=...)`; `schedule_cron`
is only for recurring mesh messages to an existing peer, not durable executor
jobs.

For unassigned path/backend jobs, each run uses a short-lived executor process
by default. Recurring jobs use `continuity=resume` to keep the backend-native
runtime session id as the continuity handle for the next fire; one-shot jobs
default to `continuity=fresh`. The process is released after terminal job
completion.

## Related

- [Scheduling](../use/features/scheduling.md)
- [Jobs](../use/features/jobs.md)
- [CLI reference](../reference/cli.md#repowire-jobs)
- [MCP tools: scheduling](../reference/mcp-tools.md#scheduling)
