# Skills

## What it does

Repowire ships its mesh usage patterns as **skills** — backend-agnostic
`SKILL.md` recipes an agent can load to coordinate over the mesh. The skills are
the single source under `skills/` in the repo and install through three
compatible channels.

The initial pack:

- **`repowire-patterns`** — reference for ask/ack vs notify, broadcast, peer
  discovery, and the cross-agent workflows. Teaching only; nothing depends on it.
- **`cross-agent-review`** — have a *different* agent backend review your work.
- **`cross-agent-plan`** — get an independent plan from a different backend.
- **`delegate`** — hand a task to a peer (reuse an existing one, or spawn with
  the user's go-ahead) and track it via the ask/ack lifecycle.
- **`repowire-install`** — install/update repowire and the skill pack from inside
  an agent session, then verify with `whoami` / `list-peers` / `doctor`.

## Backend is parameterised, never hardcoded

The cross-agent and delegate skills resolve which backend to use in this order:

1. an explicit argument the user gives,
2. the matching default from config (read via `repowire config get skills.<key>`),
3. a safe fallback — pick an available *different* backend or ask; never default
   to your own backend for a review/plan, and never hardcode one.

Defaults live in `~/.repowire/config.yaml`:

```yaml
skills:
  default_backend: codex              # generic fallback for any skill
  default_reviewer_backend: codex     # cross-agent-review
  default_planner_backend: gemini     # cross-agent-plan
  default_delegate_backend: codex     # delegate
  default_circle: default
```

`repowire config get skills.default_reviewer_backend` is a read-only seam skills
use to fetch a default without parsing the yaml; it prints nothing (exit 0) when
unset so callers fall back cleanly.

## Install channels

All three read the same `skills/` source:

- **`npx skills add`** — installs the pack (scans `skills/`) for any agent:
  ```bash
  npx skills add prassanna-ravishankar/repowire
  # or one skill:
  npx skills add https://github.com/prassanna-ravishankar/repowire/tree/main/skills/cross-agent-review
  ```
- **Claude Code plugin marketplace** — `.claude-plugin/marketplace.json` + the
  `repowire` plugin bundle the same skills:
  ```text
  /plugin marketplace add prassanna-ravishankar/repowire
  /plugin install repowire@repowire
  ```
- **`repowire-install` skill** — bootstraps/updates the pack from inside an agent
  session and verifies the mesh.

Skills are markdown recipes that call the existing `mcp__repowire__*` tools (with
CLI fallbacks). They don't change the daemon, hooks, or command semantics —
`repowire setup` remains the supported install path.
