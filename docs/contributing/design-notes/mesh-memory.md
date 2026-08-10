# Mesh memory

> **Status:** first CLI slice shipped; MCP, hook injection, edit/rm commands,
> and migration helpers remain design proposal. Builds on the orchestrator
> memory layer described in [orchestrator.md](../../concepts/orchestrator.md) and the
> persona/SOUL surface described in [personas.md](../../concepts/personas.md).

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
  the host agent's own memory implementation. Today this is CLI-only.
- Composable with the existing
  [orchestrator memory](../../concepts/orchestrator.md#memory-and-procedures) and
  [persona SOUL](../../concepts/personas.md) layers rather than replacing them.

## Non-goals

- Replacing host-agent memory systems (Claude Code's `MEMORY.md`, Cursor
  notepads, Codex memory) — those remain authoritative for their own runtime.
- Becoming a long-term recall or full session history. Detailed recall belongs
  to [session-native storage](../../concepts/session-native-roadmap.md) and SQLite-backed
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
(`daemon-go/cli/assets/orchestrator/memory/MEMORY.md`).

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
[personas.md](../../concepts/personas.md). The CLI/MCP resolves it from
`~/.repowire/orchestrator/personas/ACTIVE_PERSONA`, then falls back to the
`--persona` flag. When no persona is active, persona-scoped reads return empty
and writes require an explicit `--persona <name>`.

## Approval and write path

Mesh memory is a deliberate-action store. The contract is:

- No hook, scheduler, or background task writes memory files.
- Stop hooks, prompts, and session events MAY emit a *proposed memory*
  notification (e.g. "consider remembering: X") but the agent or operator must
  invoke an explicit `repowire memory write` (or future MCP `memory_write`) call
  to persist it.
- Memory writes always go through the public CLI/MCP surface, never via direct
  daemon side effects. This keeps an audit trail and a single chokepoint.
- The daemon does not own memory state. Storage is plain filesystem under
  `~/.repowire/memory/`; the daemon only mediates reads/writes for peers that
  prefer the HTTP/MCP route.

This is the same rule the orchestrator memory layer already implies, lifted to
the whole mesh.

The approval lifecycle is intentionally simple in the first product slice:

1. **Propose.** A peer may present a proposed memory with target scope, slug,
   type, description, body, and provenance. Provenance should point at the ask,
   session, job, or human instruction that made the memory useful.
2. **Diff.** Before any write, the operator sees the exact file-level change.
   New memories show the full file. Appends and overwrites show a unified diff
   against the existing file. Silent "learned" facts are not valid input.
3. **Decide.** The operator can approve, reject, or edit. Rejecting closes the
   proposal without changing disk. Editing creates a new diff and requires the
   same explicit approval.
4. **Write.** Only after approval does Repowire call `repowire memory write`
   (or the future MCP equivalent). Existing files still require `--append` or
   `--force`, so approval does not bypass collision protection.
5. **Record.** The write refreshes the scope `MEMORY.md` index. A future
   approval broker or jobs integration may also emit timeline/job events, but
   those records are evidence of a decision, not an alternate memory store.

The first shipped CLI slice covers the explicit write operation. It does not
yet provide a proposal queue, approval UI, or MCP approval tool. Until those
surfaces exist, the safe manual path is: draft the target Markdown, show the
diff in chat or terminal, get explicit user approval, then run
`repowire memory write`.

### Proposal shape

Future proposal surfaces should preserve these fields:

```yaml
scope: user | global | projects/<name> | personas/<name> | orchestrator
slug: short-kebab-case
type: feedback | project | user | reference | global
description: one-line summary
body: markdown body to write
provenance:
  source: ask | session | job | human | import
  ref: stable id or human-readable pointer
```

The proposal object is not itself memory. It is a pending decision. Dropping or
rejecting it must leave all memory files unchanged.

## Surfaces

### CLI

```
repowire memory list   [--scope <scope>] [--project <name>] [--persona <name>]
repowire memory show   <slug> [--scope ...]
repowire memory search <query> [--scope ...] [--all]
repowire memory write  <slug> --body "..." [--scope ...] [--type ...] [--description ...]
repowire memory write  <slug> --body "..." [--scope ...] --append
repowire memory edit   <slug> [--scope ...]      # proposed
repowire memory rm     <slug> [--scope ...]      # proposed
repowire memory path   [--scope ...]             # print resolved directory
```

Scope defaults:

- Inside an orchestrator workspace → `orchestrator`.
- Inside a registered project peer's CWD → `projects/<auto-detected-name>`.
- Otherwise → `user`.

The first shipped slice writes one Markdown file at a time, protects existing
files unless `--force` or `--append` is passed, and refreshes the scope
`MEMORY.md` index. It does not implement MCP tools yet.

`--all` on search walks every scope and prefixes results with their scope path.

### MCP

Proposed tools on the existing MCP server:

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
- **User memory** stores stable preferences about the human operator that should
  influence the whole mesh. It should stay compact and behavioral, not become a
  profile dump or private diary.
- **Global memory** stores cross-mesh operating rules that are not about the
  user personally, such as preferred commit style or escalation norms.
- **Project memory** stores durable project facts or decisions for one repo or
  workspace. Peers in other projects do not inherit it unless an orchestrator or
  human includes it in a brief.
- **Persona SOUL.md** stays as identity context, not memory. Personas may also
  accumulate memory in `personas/<name>/`, which is curated lessons distinct
  from the persona's voice/identity. Persona memory can tune how a persona works;
  it must not override user instructions, approval requirements, or permissions.
- **Host-agent memory** (Claude Code's per-project `memory/`, etc.) is
  unaffected. Operators who want a single source of truth can symlink their
  agent's memory dir into `~/.repowire/memory/projects/<name>/` themselves; the
  product does not enforce this.
- **Obsidian and vault notes** remain the right place for rich personal
  knowledge, source material, and long-form notes. Mesh memory may link or
  summarize a vault-backed decision, but approved mesh memory should contain
  only the compact rule needed by future agents.
- **Jobs** may propose memory as part of completion or review, especially when a
  job produces a durable preference or project lesson. Jobs must not write memory
  directly; they feed the same proposal, diff, approval, and explicit write path.
- **Session-native search** ([roadmap](../../concepts/session-native-roadmap.md)) covers
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
   stream) should be its own MCP tool, or whether proposals should live inside a
   general approval broker. Deferred until a real hook or job needs it.

## Implementation phasing

First CLI-only filesystem slice has landed. Suggested follow-up beads:

1. Diff-first approval helper for proposed writes. It should render the target
   scope/path and unified diff, then require explicit approve/reject/edit before
   calling `write_memory`.
2. MCP `memory_read` / `memory_write` / `memory_list` / `memory_search` tools
   wrapping the same filesystem layer.
3. SessionStart context injection block (read-only summary).
4. `repowire memory edit` / `repowire memory rm` with user-visible diffs or
   confirmations.
5. Migration helper: `repowire memory adopt` to symlink the orchestrator dir
   under `~/.repowire/memory/orchestrator`.

Each phase is independently shippable; nothing depends on auto-write or daemon
state.
