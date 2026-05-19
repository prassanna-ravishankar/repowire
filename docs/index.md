---
title: Repowire
hide:
  - navigation
---

# Repowire

Repowire is a local-first mesh for live AI coding-agent sessions. A daemon routes `ask`, `notify`, `broadcast`, and scheduled wake-ups between active Claude Code, Codex, Gemini CLI, OpenCode, and Pi sessions, plus human control surfaces such as the dashboard, Telegram, Slack, and orchestrator peers.

Use it when one repo needs a live answer from another repo, when you want to drive work from your phone or browser, or when an orchestrator peer needs to coordinate several project peers.

## Install

```bash
uv tool install repowire
```

Requires macOS or Linux, Python 3.10+, and tmux. Alternatives: `pipx install repowire`, `pip install repowire`, or the interactive installer:

```bash
curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh
```

## First ask

```bash
repowire setup
```

Open two agents in tmux windows:

```bash
# window 1
cd ~/projects/project-a && claude

# window 2
cd ~/projects/project-b && codex
```

Both sessions auto-register. In `project-a`:

> Ask project-b what API endpoints they expose.

The agent calls the `ask` MCP tool. `project-b` receives the question and acks back with `ack(corr_id, "...")`. The reply lands in `project-a` framed as `[ack #cid from @project-b] ...`.

## What to read next

- [Quickstart](quickstart/index.md) walks through install, setup, and the first cross-repo ask.
- [Concepts](concepts/index.md) covers peers, circles, message types, and the orchestrator pattern.
- [Control surfaces](surfaces/index.md) explains dashboard, Telegram, Slack, and relay control paths.
- [Patterns](patterns/index.md) covers multi-repo asks, mobile dispatch, worktree isolation, scheduled wake-ups, and orchestrator coordination.
- [MCP tools reference](reference/mcp-tools.md) is the source of truth for the agent API.
- [CLI reference](reference/cli.md) covers setup, services, peers, schedules, bots, and diagnostics.
