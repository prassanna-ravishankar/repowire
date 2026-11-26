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

## When a scheduled send cannot go out

A schedule is checked when it is created: a target this daemon has never known is refused (**404**), and one in another circle is refused (**403**) unless the sender may cross circles. A peer known by its durable record but offline (for example since a daemon restart) is accepted, since it may be back when the schedule fires.

When the schedule fires, a notify to an offline peer is queued like any notify and delivered when the peer reconnects. An **ask** is not: like a live ask, it fails when the target has no live connection, and it is not held for later, so a recurring ask to a peer that is down does not pile up stale asks for its return. Any scheduled send that fails is reported to whoever scheduled it, as a notify from `repowire:scheduler` (queued if the sender is offline, an inbox item when the sender is you). The notify names the schedule and the reason, and says whether a recurring schedule tries again at its next slot or a one-shot was removed. Each failure is also journaled as a `schedule_failed` event.

## Limits

- Schedules are message deliveries, not work records.
- A recurring schedule does not record per-run result state.
- A scheduled ask still depends on the recipient closing the ask with `ack`.
- Use jobs, not schedules, when the work should own a worker path/backend or resume runtime context between fires.
- Nothing fires while the machine sleeps. Deadline waits recheck the wall clock at intervals of at most 30 seconds of awake time, reducing delays after sleep. Delivery can take longer if other schedules or slow deliveries delay the scheduler. A recurring schedule that missed several slots fires once, then continues from the next slot after now.

## Troubleshooting

- A scheduled message did not arrive: look for a notify from `repowire:scheduler` (or a `schedule_failed` event) saying why; `repowire schedule list` shows what is still scheduled.
- Recurring work only sent a message but did not spawn an executor: use [Jobs](jobs.md) for durable recurring work.
- Ask reminders keep appearing: the recipient has not acked that scheduled ask yet.

## See also

- [Jobs](jobs.md)
- [Jobs and schedules](../../concepts/jobs-and-schedules.md)
- [CLI reference](../../reference/cli.md#repowire-schedule)
- [MCP tools](../../reference/mcp-tools.md#scheduling)
