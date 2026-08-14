# Pre-PR Hygiene

Repowire changes often touch more than one public surface. Before opening a PR, run the
advisory checklist:

```bash
scripts/pre-pr-hygiene.sh
```

The script compares your branch with `origin/main`, includes staged and unstaged changes, and
prints the documentation and repo-instruction surfaces that deserve review. It also fails fast when
tracked Beads JSONL ledgers (`.beads/issues.jsonl` or root `issues.jsonl`) appear in the committed,
staged, or unstaged diff, because those files are mutable issue state and should not ride along in
feature PRs.

To clean local-only ledger churn before opening a PR, run:

```bash
scripts/pre-pr-hygiene.sh --restore-beads-ledgers
```

That command backs up the local ledger contents under `.beads/backup/pre-pr-hygiene/` and restores
the tracked files from git. Committed ledger changes are reported only; amend or rebase those out.
The docs checklist remains advisory and is not a mandatory hook. Use judgement, keep the PR focused,
and explain any intentional docs deferral in the handoff.

## Tool-Surface Matrix

| Change area | Code paths that usually trigger review | Public surfaces to check |
| --- | --- | --- |
| CLI and setup | `daemon-go/cli/`, `daemon-go/config/`, `daemon-go/service/spawn_service.go`, `install.sh`, `repowire.yaml.example` | `README.md`, `docs/reference/cli.md`, `CLAUDE.md`, `AGENTS.md` |
| MCP surface | `daemon-go/hub/routes_mcp*.go`, `daemon-go/mcpstdio/`, `daemon-go/proto/` | `docs/reference/mcp-tools.md`, `README.md` when the surface changes materially |
| Agent runtimes and hooks | `daemon-go/hooks/`, `daemon-go/cli/assets/` | `docs/use/features/connect-*.md`, `docs/operate/transports.md`, hook/channel troubleshooting docs, `CLAUDE.md`, `AGENTS.md` |
| Dashboard and human surfaces | `web/app/dashboard/`, `daemon-go/mobile/`, attachments routes, `daemon-go/relayserver/` | `docs/use/features/`, `docs/operate/relay.md`, `README.md`, browser-generated screenshots under `images/` when UI changes materially |
| Daemon routing and architecture | `daemon-go/hub/`, `daemon-go/peer/`, `daemon-go/service/`, `daemon-go/state/` | `docs/operate/architecture.md`, `docs/concepts/`, `docs/use/workflows/`, `CLAUDE.md`, `AGENTS.md` |
