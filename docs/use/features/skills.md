# Skills

## What it is

Repowire ships mesh usage patterns as backend-agnostic `SKILL.md` recipes. Skills teach agents how to use ask/ack, notify, broadcast, peer discovery, review, planning, delegation, and installation flows without changing daemon semantics.

## When to use it

Use skills when an agent should load a repeatable workflow recipe instead of rediscovering mesh conventions from scratch.

Use the normal CLI/MCP references when you need exact command or tool schemas.

## Setup

All install channels read the same `skills/` source in the repo.

Install the pack with `npx skills`:

```bash
npx skills add prassanna-ravishankar/repowire
```

Install one skill:

```bash
npx skills add https://github.com/prassanna-ravishankar/repowire/tree/main/skills/cross-agent-review
```

Claude Code plugin marketplace installs can also bundle the same skills:

```text
/plugin marketplace add prassanna-ravishankar/repowire
/plugin install repowire@repowire
```

## Common workflows

The initial pack includes:

- `repowire-patterns` — ask/ack vs notify, broadcast, peer discovery, and cross-agent workflows.
- `cross-agent-review` — have a different agent backend review your work.
- `cross-agent-plan` — get an independent plan from a different backend.
- `delegate` — hand a task to a peer and track it via ask/ack.
- `repowire-install` — install or update Repowire and the skill pack from inside an agent session.

Cross-agent and delegate skills resolve backends in this order:

1. Explicit user argument.
2. Matching config default from `repowire config get skills.<key>`.
3. Safe fallback to an available different backend, or ask for direction.

## Commands and API

Defaults live in `~/.repowire/config.yaml`:

```yaml
skills:
  default_backend: codex
  default_reviewer_backend: codex
  default_planner_backend: pi
  default_delegate_backend: codex
  default_circle: default
```

`repowire config get skills.default_reviewer_backend` is the read-only seam skills use to fetch defaults without parsing YAML directly.

## Limits

- Skills call existing `mcp__repowire__*` tools with CLI fallbacks.
- Skills do not change daemon, hook, or command semantics.
- `repowire setup` remains the supported install path for Repowire itself.
- Backend choices should be parameterized; review/plan skills should not default to the current backend for independent review.

## Troubleshooting

- A skill picked the wrong backend: inspect the explicit invocation and `skills.*` config defaults.
- A skill cannot find Repowire tools: confirm `repowire setup` installed the MCP server for that runtime.
- A marketplace install drifts from the repo skill pack: reinstall the plugin or skill pack from the current source.

## See also

- [Cross-agent review workflow](../workflows/cross-agent-review.md)
- [Claude Code plugin packaging](../../contributing/design-notes/claude-code-plugin-packaging.md)
- [MCP tools](../../reference/mcp-tools.md)
