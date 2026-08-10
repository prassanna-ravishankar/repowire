# Documentation Standards

Feature work that changes public behavior must update public docs in the same PR.

## Where changes go

- README: install path, quickstart, supported agents, major features, screenshots, roadmap positioning.
- Use: active-user feature pages and workflow recipes.
- Concepts: mental models and invariants.
- Operate: daemon, relay, transports, state, deployment, and security.
- Reference: exact CLI, MCP, config, HTTP, WebSocket, and hook details.
- Troubleshooting: symptom-oriented fixes.
- Contributing: maintainer workflows, release/versioning, backend additions, and design notes.

## Before opening a PR

```bash
uvx --from zensical==0.0.43 zensical build --strict
scripts/pre-pr-hygiene.sh
```

If docs are intentionally deferred, file a Beads follow-up and say why in the PR handoff.

## Related

- [Pre-PR hygiene](pre-pr-hygiene.md)
- [Design system](design-system.md)
