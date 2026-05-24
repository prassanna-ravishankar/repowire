# Mesh memory

> **Status:** design proposal. No code shipped under this name yet. Builds on the
> orchestrator memory layer described in [orchestrator.md](orchestrator.md) and
> the persona/SOUL surface described in [personas.md](personas.md).

Repowire today only ships one curated memory surface: the orchestrator
workspace at `~/.repowire/orchestrator/memory/`. Every other peer either keeps
its memory inside a host-agent specific location (Claude Code's per-project
`memory/`, Codex notes, vault folders) or writes nothing durable at all.

Mesh memory generalises the orchestrator's curated-procedure pattern to the
whole mesh: project peers, personas, users, and global mesh conventions all get
the same file-based memory shape, addressable from the same CLI/MCP surfaces,
without changing the orchestrator's existing role.

## Goals

- One memory shape across the mesh — same frontmatter, same `MEMORY.md` index
  pattern, same write/read semantics — so an operator can move between
  orchestrator, project peers, and personas without re-learning a system.
- Cleanly namespaced storage that survives backups, sync, and re-installs.
- Deliberate writes only. Hooks and prompts may *suggest* a memory write but
  must never silently mutate memory state.
- A small CLI/MCP read+write surface that any peer can use without depending on
  the host agent's own memory implementation.
- Composable with the existing
  [orchestrator memory](orchestrator.md#memory-and-procedures) and
  [persona SOUL](personas.md) layers rather than replacing them.

## Non-goals

- Replacing host-agent memory systems (Claude Code's `MEMORY.md`, Cursor
  notepads, Codex memory) — those remain authoritative for their own runtime.
- Becoming a long-term recall or full session history. Detailed recall belongs
  to [session-native storage](session-native-roadmap.md) and SQLite-backed
  search.
- Implementing a vector store, embeddings, or any retrieval beyond grep/keyword.
  Mesh memory is curated text, not a knowledge base.
- Auto-writing or summarising on the agent's behalf. See "No auto-writes" below.

## Namespace

All mesh memory lives under `~/.repowire/memory/`:

```
~/.repowire/memory/
├── MEMORY.md                  # top-level pointer index (optional)
├── global/                    # cross-mesh conventions and operator rules
│   ├── MEMORY.md
│   └── <slug>.md
├── user/                      # the operator behind the mesh
│   ├── MEMORY.md
│   └── <slug>.md
├── projects/
│   └── <project-name>/
│       ├── MEMORY.md
│       └── <slug>.md
├── personas/
│   └── <persona-name>/
│       ├── MEMORY.md
│       └── <slug>.md
└── orchestrator/              # symlink to ~/.repowire/orchestrator/memory/
```

Each scope has the same internal shape: a `MEMORY.md` index plus one file per
memory, identical frontmatter to the orchestrator template
(`repowire/orchestrator/template/memory/MEMORY.md`).

### Scope semantics

| Scope | Owner | Read by | Example |
|-------|-------|---------|---------|
| `global/` | mesh operator | every peer in the mesh | "Default to short commit messages." |
| `user/` | mesh operator | every peer, but framed as preferences about the human | "Operator is a senior backend engineer, prefers Go analogies." |
| `projects/<name>/` | project peers on `<name>` | peers whose `project` matches | "Auth middleware rewrite is compliance-driven, not tech debt." |
| `personas/<name>/` | a persona | sessions running that persona | "Anya answers concisely, refuses to predict timelines." |
| `orchestrator/` | the orchestrator role | the active orchestrator session | unchanged from today |

The orchestrator entry is a symlink, not a moved directory, so the existing
workspace at `~/.repowire/orchestrator/memory/` keeps working untouched. This
preserves the orchestrator's "workspace owns its own memory" property while
making it discoverable from `~/.repowire/memory/orchestrator`.

### Project resolution

A peer's project name comes from its existing registry entry (`project` column
in `list_peers`). The CLI/MCP resolves the scope path as
`~/.repowire/memory/projects/<peer.project>/`. If the directory does not exist,
read returns empty and write creates it on first use.

### Persona resolution

A persona's name comes from the active persona marker described in
[personas.md](personas.md). The CLI/MCP resolves it from
`~/.repowire/orchestrator/personas/ACTIVE_PERSONA`, then falls back to the
`--persona` flag. When no persona is active, persona-scoped reads return empty
and writes require an explicit `--persona <name>`.

## No auto-writes

Mesh memory is a deliberate-action store. The contract is:

- No hook, scheduler, or background task writes memory files.
- Stop hooks, prompts, and session events MAY emit a *proposed memory*
  notification (e.g. "consider remembering: X") but the agent or operator must
  invoke an explicit `repowire memory write` (or MCP `memory_write`) call to
  persist it.
- Memory writes always go through the public CLI/MCP surface, never via direct
  daemon side effects. This keeps an audit trail and a single chokepoint.
- The daemon does not own memory state. Storage is plain filesystem under
  `~/.repowire/memory/`; the daemon only mediates reads/writes for peers that
  prefer the HTTP/MCP route.

This is the same rule the orchestrator memory layer already implies, lifted to
the whole mesh.

## Surfaces

### CLI

```
repowire memory list   [--scope <scope>] [--project <name>] [--persona <name>]
repowire memory show   <slug> [--scope ...]
repowire memory search <query> [--scope ...] [--all]
repowire memory write  <slug> --body "..." [--scope ...] [--type ...] [--description ...]
repowire memory append <slug> --body "..." [--scope ...]
repowire memory edit   <slug> [--scope ...]      # opens $EDITOR
repowire memory rm     <slug> [--scope ...]
repowire memory path   [--scope ...]             # print resolved directory
```

Scope defaults:

- Inside an orchestrator workspace → `orchestrator`.
- Inside a registered project peer's CWD → `projects/<auto-detected-name>`.
- Otherwise → `user` for read commands, refuse with a hint for write commands.

`--all` on search walks every scope and prefixes results with their scope path.

### MCP

Three new tools on the existing MCP server:

- `memory_list(scope?, project?, persona?) -> TSV(slug, type, description, updated_at)`
- `memory_read(slug, scope?, project?, persona?) -> str` — returns the file body
- `memory_write(slug, body, scope?, project?, persona?, type?, description?, append=False) -> str`
- `memory_search(query, scope?, all=False) -> TSV(scope, slug, snippet)`

All tools default scope from the caller's identity (same rules as the CLI). The
MCP layer never auto-summarises or auto-writes; `memory_write` is the only path
that mutates state.

### Hooks (read-only)

The existing SessionStart context injection gains a small, optional
`[Repowire Memory]` block summarising which scopes have entries and how to read
them, mirroring how persona context is injected today. The block is purely
informational; it never embeds memory bodies inline to avoid context bloat.

## Format

Identical to the orchestrator memory template:

```markdown
---
name: <short-kebab-case-slug>
description: <one-line summary>
metadata:
  type: feedback | project | user | reference | global
---

# <short title>

**Why:** <the incident or strong preference behind the rule>

**How to apply:** <when/where this kicks in>
```

`MEMORY.md` per scope is a one-line-per-entry index, capped at the same
~150-line soft budget the orchestrator template uses.

## Relation to existing layers

- **Orchestrator memory** keeps its workspace path and template. The new
  `~/.repowire/memory/orchestrator` is just a symlink for discovery.
- **Persona SOUL.md** stays as identity context, not memory. Personas may also
  accumulate memory in `personas/<name>/`, which is curated lessons distinct
  from the persona's voice/identity.
- **Host-agent memory** (Claude Code's per-project `memory/`, etc.) is
  unaffected. Operators who want a single source of truth can symlink their
  agent's memory dir into `~/.repowire/memory/projects/<name>/` themselves; the
  product does not enforce this.
- **Session-native search** ([roadmap](session-native-roadmap.md)) covers
  detailed recall. Mesh memory remains the curated layer.

## Open questions

1. Should `global/` and `user/` collapse into one scope? Keeping them separate
   matches the auto-memory taxonomy and lets us tag user-profile facts
   distinctly from cross-mesh procedural rules.
2. Sync. The orchestrator workspace is single-machine today. If/when we add
   cross-machine sync, mesh memory inherits the same questions; out of scope
   for this design.
3. Conflict resolution between scopes when a peer reads from multiple. Initial
   answer: the CLI lists all matches; the agent decides. No automatic merge.
4. Whether `memory_propose` (a read-only "I noticed something worth saving"
   stream) should be its own MCP tool. Deferred until a real hook needs it.

## Implementation phasing

Docs-only first (this file). Suggested follow-up beads:

1. `repowire memory` CLI subcommand with filesystem-only read/write.
2. MCP `memory_read` / `memory_write` / `memory_list` / `memory_search` tools
   wrapping the same filesystem layer.
3. SessionStart context injection block (read-only summary).
4. Migration helper: `repowire memory adopt` to symlink the orchestrator dir
   under `~/.repowire/memory/orchestrator`.

Each phase is independently shippable; nothing depends on auto-write or daemon
state.
