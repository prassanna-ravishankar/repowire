# Orchestration

## What it is

Orchestration is using one peer as a coordinator for other peers. An orchestrator dispatches asks, collects status, routes reviews, schedules check-ins, and keeps multi-repo or multi-agent work moving.

## When to use it

Use orchestration when work spans multiple peers, repositories, backends, or time windows and needs explicit coordination. Use direct `ask` or `notify_peer` for one-off messages that do not need a coordinator.

## Setup

Any normal agent session can act as an orchestrator. A long-running coordinator should claim the orchestrator role:

```bash
repowire peer claim-role orchestrator
```

Persona helpers can make routing policy explicit:

```bash
repowire orchestrator persona ...
```

## Common workflows

- Claim or repair the orchestrator role for a coordinator peer.
- Use `orchestrator_status` before dispatching long-running work.
- Dispatch closure-sensitive handoffs with `ask`.
- Use [Jobs](jobs.md) when delegated work needs durable lifecycle and result tracking.
- Route review work through `review_queue` and `mark_reviewed`.
- Schedule check-ins so the orchestrator wakes itself or workers later.
- Pair the orchestrator with a persona when routing policy should be explicit.

## Commands and API

- CLI: `repowire peer claim-role orchestrator`, `repowire orchestrator persona ...`.
- MCP: `orchestrator_status`, `review_queue`, `mark_reviewed`, plus the normal `ask`, `notify_peer`, scheduling, and job tools. Orchestrator role repair is CLI-only.

## Limits

- The orchestrator is a workflow role, not a magic global scheduler.
- It works best when the human gives explicit goals, scope, handoff expectations, and closure criteria.
- Long-running coordination still depends on peers acking asks or updating jobs.
- Role data is mesh state; do not infer routing-sensitive identity from display names.

## Troubleshooting

- Cross-circle asks are unexpectedly blocked: check the resolved peer identity, role, and circle with `whoami` / `list_peers`.
- Handoffs are lost: use tracked asks or jobs rather than untracked notifications.
- Status is stale: run `orchestrator_status`, inspect open asks, and check for ghost peers.

## See also

- [Orchestrator pattern](../../concepts/orchestrator.md)
- [Personas](../../concepts/personas.md)
- [Orchestrator coordination workflow](../workflows/orchestrator-coordination.md)
- [CLI reference](../../reference/cli.md#repowire-orchestrator-persona)
- [MCP tools](../../reference/mcp-tools.md#orchestrator_status)
