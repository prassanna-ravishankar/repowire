---
title: Repowire
hide:
  - navigation
---

# Repowire

Mesh network for AI coding agents. A local-first daemon routes `ask`, `notify`, and `broadcast` between active Claude Code, Codex, Gemini CLI, and OpenCode sessions.

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

## Where to next

- [Quickstart](quickstart/index.md) walks through install, setup, and the first cross-repo ask.
- [Concepts](concepts/index.md) covers peers, circles, message types, and the orchestrator pattern.
- [MCP tools reference](reference/mcp-tools.md) is the source of truth for the agent API.
- [Troubleshooting](troubleshooting/index.md) for when hooks or the daemon misbehave.
