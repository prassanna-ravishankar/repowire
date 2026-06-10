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

## Fire lifecycle: structural completion

A job fire's result is captured by the daemon, not self-reported by the
executor. The dispatch prompt (sent as an ask from the built-in `@jobs`
service peer) embeds `job_id`/`attempt_id` markers; the Stop hook posts the
executor's turn pair as chat turns, and the daemon correlates them:

- The user-role turn carrying the marker **arms** the fire (`running` /
  phase `turn_started`).
- The next assistant-role turn from the executor **completes** it: the turn's
  final message becomes `result_summary` (full text in
  `result_data.final_message`), the dispatch ask is auto-closed, and the
  per-fire executor is released.
- An executor that dies with an in-flight fire (terminal peer offline,
  daemon-restart reconcile) fails it with `executor_died`.

One fire = one turn. A job that needs a peer's answer must wait *inside* its
turn with `wait_on_ack`, never by ending the turn. The executor contract is
spelled out in the dispatch prompt: there is no human at the terminal, and the
turn ending ends the fire.

`job_update` is optional enrichment — progress notes and structured
`result_data`. The one escape hatch: a fire blocked on something outside the
mesh can hold itself open across turns with `job_update state=running` and an
explicit `phase`; it must then terminal-report itself.

Only the runner's own `dispatching` phase is lease-bounded. A `delivered`
fire belongs to the executor and is never reaped by a wall clock — executor
death is the liveness signal.

## Observability

Every work state transition emits a `job_state_changed` event. Terminal
states additionally send a best-effort one-line notify from `@jobs` to the
job's owner — `owner_peer_id`, falling back to the creator, then the circle's
orchestrator. Jobs never depend on any of those being alive: cron fires
regardless, and the durable work row (`job_status` / `job_result`) is the
source of truth whenever someone comes asking.

## Related

- [Scheduling](../use/features/scheduling.md)
- [Jobs](../use/features/jobs.md)
- [CLI reference](../reference/cli.md#repowire-jobs)
- [MCP tools: scheduling](../reference/mcp-tools.md#scheduling)
