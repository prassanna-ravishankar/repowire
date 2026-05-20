# Memory Index

Persistent operational memory. Each entry below points to a file in this directory.

This is the curated procedural layer, not the long-term recall layer. Use it for lessons that should change future orchestration behavior. Use session history/search, issue notes, or commit messages for detailed history, status, and "what happened when" recall.

User communication and routing preferences belong in `../comms.md`. Active project scope belongs in `../projects.md`.

## Budget

- Keep this index compact and scannable; soft limit: about 150 lines.
- Keep each memory file focused; soft limit: about 1200 characters.
- Consolidate when two memories overlap or when the index stops being easy to scan.
- Prefer patch-style updates with a short rationale over full rewrites.

## Format

Each memory file should be self-contained, with this structure:

```markdown
# <short title>

**Why:** <the incident or strong preference behind the rule. Why are we doing this?>

**How to apply:** <when/where this kicks in. Default to "default X UNLESS Y" framing, not "always X".>
```

## Index

<!-- One line per memory file. Format:
- [Short title](filename.md) — one-line description of when this applies
-->

_(no memories yet; this index grows as the orchestrator learns)_

## When to add a memory

- The user corrects an approach with "next time X, do Y differently" → save it
- A non-obvious lesson surfaces from a real incident → save it with the incident as the Why
- A judgment call gets validated ("yes, the bundled PR was the right call here") → save it

## When NOT to add a memory

- One-off events with no forward-applicable lesson → log in commit / bd note instead
- Things that are already documented in the codebase (in AGENTS.md, patterns/, or the project's own docs) → reference, don't duplicate
- Counts, status updates, recent work — that's ephemeral, not durable knowledge

## Maintenance

When a memory becomes stale (the underlying system or preference changes), update it rather than letting it rot. When two memories cover the same ground from slightly different angles, consolidate them. The index is the contract — keep it readable.
