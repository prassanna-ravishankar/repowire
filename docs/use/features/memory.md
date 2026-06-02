# Memory

## What it is

Mesh memory is the direction for explicit, reviewable knowledge writes that help orchestrators and persona sessions carry durable lessons across turns.

## When to use it

Use memory when a lesson, preference, project fact, or coordination rule should survive the current conversation and be available to future mesh work.

Use normal asks, job notes, or docs when the information is temporary, private, or not ready to become shared project knowledge.

## Setup

Memory is still an evolving capability. Treat the contributor design note and CLI reference as the source of truth for what is currently shipped.

## Common workflows

- Record a reusable project or coordination lesson after a long session.
- Keep orchestrator preferences explicit instead of relying on the agent to remember them.
- Review memory writes before making them durable.

## Commands and API

Use the shipped CLI surface documented in [CLI reference](../../reference/cli.md#repowire-memory). The design direction is documented in [Mesh memory](../../contributing/design-notes/mesh-memory.md).

## Limits

- Do not treat memory as a private secret store.
- Public repo memory should avoid secrets, absolute personal paths, hostnames, and personal identifiers.
- If the CLI reference does not document the command behavior you want, treat it as not shipped yet.

## Troubleshooting

- A memory did not influence an orchestrator turn: confirm the recall or triage path surfaced it at decision time, not merely that it exists on disk.
- A memory is too sensitive for project docs: keep it out of committed memory and use a narrower private channel.

## See also

- [Mesh memory design note](../../contributing/design-notes/mesh-memory.md)
- [CLI reference](../../reference/cli.md#repowire-memory)
