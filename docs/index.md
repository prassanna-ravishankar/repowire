---
title: Repowire
hide:
  - navigation
---

# Repowire

Repowire is a local-first harness for working with more than one coding agent at a time. It gives every live Claude Code, Codex, Gemini CLI, OpenCode, or Pi session an address in a shared mesh, so agents can ask each other questions, send updates, schedule follow-ups, and coordinate without copy-paste handoffs.

Think of it as the lightweight operating layer around your agent team: a communication mesh, an orchestrator path for multi-repo work, and a set of human controls for when you want to steer from a browser, Telegram, or Slack.

Use it when one repo needs a concrete answer from another repo, when you want a personal orchestrator session to dispatch tasks and collect status, or when you want to monitor and nudge agent work from your phone or browser.

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

Claude Code registers on session start. Codex registers after its first interaction, so send a short warmup prompt in `project-b`, then confirm both peers with `repowire peer list`. In `project-a`:

> Ask project-b what API endpoints they expose.

The agent calls the `ask` MCP tool. `project-b` receives the question and acks back with `ack(corr_id, "...")`. The reply lands in `project-a` framed as `[ack #cid from @project-b] ...`.

Repowire is not a standalone chat UI in this flow. You ask your local agent in natural language, and that agent invokes Repowire's MCP tools.

## What to read next

- [Quickstart](quickstart/index.md) walks through install, setup, and the first cross-repo ask.
- [Concepts](concepts/index.md) covers peers, circles, message types, and the orchestrator pattern.
- [Control surfaces](surfaces/index.md) explains dashboard, Telegram, Slack, and relay control paths.
- [Patterns](patterns/index.md) covers multi-repo asks, mobile dispatch, worktree isolation, scheduled wake-ups, and orchestrator coordination.
- [MCP tools reference](reference/mcp-tools.md) is the source of truth for the agent API.
- [CLI reference](reference/cli.md) covers setup, services, peers, schedules, bots, and diagnostics.
