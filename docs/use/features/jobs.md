# Jobs

## What it is

Jobs are durable tracked work records. They let humans and agents create work, inspect status, update progress, record results, cancel work, and run recurring worker templates.

## When to use it

Use jobs when work needs lifecycle state that survives across turns: queued/running/succeeded/failed/canceled state, result summaries, cancellation, retry, dashboard inspection, or recurring path/backend workers.

Use `ask` for one question that needs a reply. Use [Scheduling](scheduling.md) for future mesh messages that do not need durable work state.

## Setup

Start the daemon. For executor-backed jobs, configure the backend command and allowed path through spawn configuration.

```bash
repowire serve
```

Worker folders can live under `.repowire/agents/<name>`:

```bash
repowire agents create daily-brief
```

## Common workflows

Create a recurring worker job:

```bash
repowire jobs create "Daily brief" \
  --path .repowire/agents/daily-brief \
  --backend codex \
  --cron "@daily" \
  --prompt "Prepare the brief."
```

Inspect jobs:

```bash
repowire jobs list
repowire jobs show job-...
```

Update progress or record a result:

```bash
repowire jobs update job-... --state running --note "Started first pass"
repowire jobs update job-... --state succeeded --result-summary "Brief posted"
repowire jobs result job-...
```

Cancel work that should not continue:

```bash
repowire jobs cancel job-...
```

Unassigned path/backend jobs run with per-fire executors by default: Repowire spawns or backend-resumes an executor for the run, delivers the job ask, and releases the executor after a terminal job update. Recurring jobs can use `continuity=resume` to keep the backend-native runtime session id as the continuity handle for the next fire. Use `--continuity fresh` when a run should start without prior runtime context.

## Commands and API

- CLI: `repowire jobs create`, `list`, `show`, `update`, `result`, `cancel`.
- MCP: `job_create`, `job_list`, `job_status`, `job_show`, `job_update`, `job_result`, `job_cancel`.
- Dashboard: Jobs view for listing durable work and recurring templates, then running, retrying, or canceling eligible records.

`schedule_cron` is only for recurring mesh messages to an existing peer. Use `job_create(..., cron=...)` or `repowire jobs create --cron ...` for recurring durable executor work.

## Limits

- Jobs complement asks and schedules; they do not replace the ask/ack lifecycle for conversations.
- Per-fire executor cleanup is tied to terminal job lifecycle state, not ack lifecycle.
- Ack only confirms receipt. Terminal job updates and compatible cancellation paths release the executor.
- Resume continuity depends on a backend that supports local resume and a captured runtime session id.

## Troubleshooting

- A recurring job did not create another run: inspect the job template with `repowire jobs show` and confirm it was created with `--cron`.
- A worker did not resume prior context: confirm the job continuity is `resume`, the first run captured a runtime session id, and the backend supports resume.
- A process stayed alive after work ended: confirm the job reached a terminal state with `jobs update` or `jobs cancel`.

## See also

- [Scheduling](scheduling.md)
- [Jobs and schedules](../../concepts/jobs-and-schedules.md)
- [CLI reference](../../reference/cli.md#repowire-jobs)
- [MCP tools](../../reference/mcp-tools.md#lifecycle)
