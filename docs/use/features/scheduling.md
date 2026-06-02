# Scheduling

## What it is

Scheduling delivers future mesh messages. A schedule can send a one-shot reminder, a future ask, or a recurring cron delivery to yourself, another peer, or an orchestrator.

## When to use it

Use schedules when the work is message-oriented: reminders, check-ins, future asks, daily nudges, and lightweight recurring prompts.

Use [Jobs](jobs.md) when the work needs durable status, results, cancellation, recurring worker templates, or backend executor continuity.

## Setup

Start the daemon. No separate scheduler process is required.

```bash
repowire serve
```

Schedules are stored in daemon state and delivered by the normal mesh routing paths when due.

## Common workflows

Schedule a self-reminder:

```bash
repowire schedule self 10m "check CI"
```

Schedule a future ask to an orchestrator:

```bash
repowire schedule create orchestrator 1h "handoff" --from-peer project-a --kind ask
```

Schedule a recurring notification:

```bash
repowire schedule cron orchestrator "@daily" "review open jobs"
```

List and delete schedules:

```bash
repowire schedule list
repowire schedule delete sched-...
```

Use `kind=ask` only when each delivery should require an explicit `ack`. Open asks continue to resurface through the ask reminder path until closed.

## Commands and API

- CLI: `repowire schedule self`, `repowire schedule create`, `repowire schedule cron`, `repowire schedule list`, `repowire schedule delete`.
- MCP: `schedule_create`, `schedule_self`, `schedule_cron`, `schedule_list`, `schedule_delete`.

Schedules deliver mesh messages to existing peers. They do not create durable executor jobs.

## Limits

- Schedules are message deliveries, not work records.
- A recurring schedule does not record per-run result state.
- A scheduled ask still depends on the recipient closing the ask with `ack`.
- Use jobs, not schedules, when the work should own a worker path/backend or resume runtime context between fires.

## Troubleshooting

- A scheduled message did not arrive: run `repowire schedule list`, confirm the target peer exists, and check daemon logs.
- Recurring work only sent a message but did not spawn an executor: use [Jobs](jobs.md) for durable recurring work.
- Ask reminders keep appearing: the recipient has not acked that scheduled ask yet.

## See also

- [Jobs](jobs.md)
- [Jobs and schedules](../../concepts/jobs-and-schedules.md)
- [CLI reference](../../reference/cli.md#repowire-schedule)
- [MCP tools](../../reference/mcp-tools.md#scheduling)
