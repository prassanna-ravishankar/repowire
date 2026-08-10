# Contributing to Repowire

Thanks for wanting to contribute! Here's everything you need to get started.

## Getting Started

A good place to start is the [`good first issue`](https://github.com/prassanna-ravishankar/repowire/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) label on GitHub. These are scoped tasks suited for new contributors.

## Setting Up the Dev Environment

You'll need Go 1.25+, Node.js 22+, and [bun](https://bun.sh/) only if you're touching the experimental channel server.
```bash
# Clone the repo
git clone https://github.com/prassanna-ravishankar/repowire.git
cd repowire

# Build the dashboard and native binary
cd web && npm ci && npm run build && cd ..
mkdir -p bin
(cd daemon-go && go build -o ../bin/repowire .)
./bin/repowire setup --non-interactive
```

If you're working on the channel server:
```bash
cd daemon-go/cli/assets/channel && bun install
```

If you're working on the dashboard (`web/`):
```bash
cd web && npm install && npm run dev   # dev server
repowire build-ui                      # production build (served by daemon at /dashboard)
```

## Running Tests and Linting

Before pushing anything, make sure these all pass:
```bash
cd daemon-go && gofmt -w . && go vet ./... && go test -race ./...
cd web && npm test -- --run && npm run build
```

CI runs the same gates and cross-compiles all release targets on every PR.

## How Hooks Work

This is the most common gotcha for new contributors: hooks run from the **binary recorded by setup**, not directly from source files. Rebuild it after changes:
```bash
(cd daemon-go && go build -o ../bin/repowire .)
./bin/repowire setup --non-interactive
```

If your changes aren't showing up, this is almost always why.

## Code Style

Repowire uses `gofmt` and `go vet` for native code. Format before committing:
```bash
cd daemon-go && gofmt -w . && go vet ./...
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

Before opening the PR, run the advisory repo-hygiene checklist:
```bash
scripts/pre-pr-hygiene.sh
```

This is not a mandatory hook. It compares your branch with `origin/main` and reminds you which
public surfaces to check: README, reference docs, mirrored web docs, `CLAUDE.md` / `AGENTS.md`,
and graphify for architecture-level changes. See [`docs/pre-pr-hygiene.md`](docs/pre-pr-hygiene.md)
for the tool-surface matrix.

## Where to Find Things

`CLAUDE.md` has the full architecture overview, worth reading before diving in. Here's a quick map of the main areas:

| Module | What it does |
|---|---|
| `daemon-go/hub/`, `peer/`, `service/` | Central routing hub, peer registry, ask lifecycle, and HTTP/WebSocket routes |
| `daemon-go/hooks/` | Native session, stop, prompt, notification, and ws-hook transport |
| `daemon-go/cli/assets/channel/` | Experimental MCP stdio transport (requires bun) |
| `daemon-go/mcpstdio/` | Per-session stdio identity shim for daemon-owned HTTP MCP tools |
| `daemon-go/relayserver/` | Hosted relay at repowire.io (WS bridge + HTTP tunnel) |
| `daemon-go/mobile/` | Native Telegram and Slack bot peers |
| `web/` | Next.js dashboard, build with `repowire build-ui` |

Repowire follows a **lazy repair** philosophy. Nothing polls. Work is deferred until needed and piggy-backed on incoming requests. Avoid adding polling loops, periodic timers, or eager disk writes.

## Questions?

Open an issue if you get stuck or need guidance.
