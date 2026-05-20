# Pre-PR Hygiene

Repowire changes often touch more than one public surface. Before opening a PR, run the
advisory checklist:

```bash
python3 scripts/pre_pr_hygiene.py
```

The script compares your branch with `origin/main`, includes staged and unstaged changes, and
prints the documentation and repo-instruction surfaces that deserve review. It is intentionally
not a mandatory hook. Use judgement, keep the PR focused, and explain any intentional docs deferral
in the handoff.

## Tool-Surface Matrix

| Change area | Code paths that usually trigger review | Public surfaces to check |
| --- | --- | --- |
| CLI and setup | `repowire/cli.py`, `repowire/config/`, `repowire/spawn.py`, `install.sh`, `repowire.yaml.example` | `README.md`, `docs/reference/cli.md`, `web/app/docs/reference/cli/page.tsx`, `CLAUDE.md`, `AGENTS.md` |
| MCP and Python client | `repowire/mcp/`, `repowire/peer_mcp.py`, `repowire/client.py`, `repowire/protocol/` | `docs/reference/mcp-tools.md`, `docs/reference/python-client.md`, mirrored web docs, `README.md` when the surface changes materially |
| Agent runtimes and hooks | `repowire/hooks/`, `repowire/installers/`, `repowire/channel/`, `repowire/acp/` | `docs/agents/`, hook/channel troubleshooting docs, `CLAUDE.md`, `AGENTS.md` |
| Dashboard and human surfaces | `web/app/dashboard/`, `repowire/telegram/`, `repowire/slack/`, attachments routes, relay code | `docs/surfaces/`, `docs/relay/`, `web/app/docs/`, `README.md`, browser-generated screenshots under `images/` when UI changes materially |
| Daemon routing and architecture | `repowire/daemon/`, `repowire/session/`, routing/lifecycle/scheduling state | `docs/reference/architecture.md`, `docs/concepts/`, `docs/patterns/`, `CLAUDE.md`, `AGENTS.md` |

## Graphify Reminder

For architecture-level changes, especially daemon routing, peer state, hook lifecycle, transport,
or session model changes, run the incremental graph update:

```bash
/graphify . --update
```

Use `graphify-out/GRAPH_REPORT.md` as a navigation aid for the PR summary when helpful. Do not
paste large generated JSON or cache artifacts into README or hand-written docs.
