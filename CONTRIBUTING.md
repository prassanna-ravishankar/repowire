# Contributing to Repowire

Thanks for wanting to contribute! Here's everything you need to get started.

## Getting Started

A good place to start is the [`good first issue`](https://github.com/prassanna-ravishankar/repowire/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) label on GitHub. These are scoped tasks suited for new contributors.

## Setting Up the Dev Environment

You'll need Python 3.10+, [uv](https://docs.astral.sh/uv/getting-started/installation/), and [bun](https://bun.sh/) if you're touching the channel server.
```bash
# Clone the repo
git clone https://github.com/prassanna-ravishankar/repowire.git
cd repowire

# Install dev dependencies (pytest, ruff, ty, httpx-ws)
uv sync --extra dev --group dev

# Install repowire globally (hooks run from the installed package, not source)
uv tool install --force --reinstall .
```

If you're working on the channel server:
```bash
cd repowire/channel && bun install
```

If you're working on the dashboard (`web/`):
```bash
cd web && npm install && npm run dev   # dev server
repowire build-ui                      # production build (served by daemon at /dashboard)
```

## Running Tests and Linting

Before pushing anything, make sure these all pass:
```bash
uv run pytest                  # run tests
uv run ruff check repowire/   # lint
uv run ty check repowire/     # type check
```

CI runs all three on every PR, so it's easier to catch issues locally first.

## How Hooks Work

This is the most common gotcha for new contributors: hooks run from the **installed package**, not your source files. After any code change, reinstall before your changes take effect:
```bash
uv tool install --force --reinstall .
```

If your changes aren't showing up, this is almost always why.

## Code Style

Repowire uses [ruff](https://docs.astral.sh/ruff/) with a line length of 100. The full config is in `pyproject.toml`. You can auto-fix most issues with:
```bash
uv run ruff check repowire/ --fix
```

## PR Workflow

Fork the repo, create a branch, make your changes, and open a PR against `main`. Try to keep PRs focused on one thing.
```bash
git checkout -b your-branch-name
# make your changes
git add <files>
git commit -m "short description of what and why"
git push origin your-branch-name
```

## Where to Find Things

`CLAUDE.md` has the full architecture overview, worth reading before diving in. Here's a quick map of the main areas:

| Module | What it does |
|---|---|
| `daemon/` | Central routing hub: peer registry, message router, ask tracker (non-blocking ask lifecycle), legacy query tracker, HTTP routes |
| `hooks/` | Default agent transport (Claude, Codex, Gemini): session, stop, prompt, notification handlers + ask-pickup transport reporter |
| `channel/server.ts` | Experimental MCP stdio transport (requires bun) |
| `mcp/server.py` | MCP tools: `list_peers`, `ask`, `ack`, `notify_peer`, `broadcast`, etc. |
| `relay/server.py` | Hosted relay at repowire.io (WS bridge + HTTP tunnel) |
| `telegram/bot.py` | Telegram bot peer for mobile mesh control |
| `slack/bot.py` | Slack bot peer via Socket Mode |
| `web/` | Next.js dashboard, build with `repowire build-ui` |

Repowire follows a **lazy repair** philosophy. Nothing polls. Work is deferred until needed and piggy-backed on incoming requests. Avoid adding polling loops, periodic timers, or eager disk writes.

## Questions?

Open an issue if you get stuck or need guidance.
