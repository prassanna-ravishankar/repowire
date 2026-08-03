---
title: Repowire
---

# Repowire

Repowire is a local-first harness for working with more than one coding agent at a time. It gives every live Claude Code, Codex, Gemini CLI, OpenCode, or Pi session an address in a shared mesh, so agents can ask each other questions, send updates, schedule follow-ups, and coordinate without copy-paste handoffs.

Think of it as the lightweight operating layer around your agent team: a communication mesh, an orchestrator path for multi-repo work, and a set of human controls for when you want to steer from a browser, Telegram, or Slack.

Use it when one repo needs a concrete answer from another repo, when you want a personal orchestrator session to dispatch tasks and collect status, or when you want to monitor and nudge agent work from your phone or browser.

## Explore the docs

<div class="doc-card-grid">
  <a class="doc-card" href="start/">
    <strong>Start</strong>
    <span>Install, run setup, and send the first cross-repo ask.</span>
  </a>
  <a class="doc-card" href="use/">
    <strong>Use</strong>
    <span>Features and workflows for active mesh work.</span>
  </a>
  <a class="doc-card" href="concepts/">
    <strong>Concepts</strong>
    <span>Peers, sessions, backends, transports, messages, jobs, and routing.</span>
  </a>
  <a class="doc-card" href="operate/">
    <strong>Operate</strong>
    <span>Daemon, relay, transports, state, auth, deployment, and architecture.</span>
  </a>
  <a class="doc-card" href="reference/">
    <strong>Reference</strong>
    <span>MCP tools, CLI commands, config, HTTP, WebSocket, and hooks.</span>
  </a>
</div>

## Install

```bash
curl -fsSL https://github.com/prassanna-ravishankar/repowire/releases/latest/download/install.sh | sh
```

Requires macOS or Linux and tmux. The installer downloads a checksum-verified native release; Python is not required. See [Install](start/install.md).

## First ask

Open two agents in tmux windows:

```bash
# window 1
cd ~/projects/project-a && claude

# window 2
cd ~/projects/project-b && codex
```

Claude Code registers on session start. Codex registers through App Server when
its TUI thread opens, before the first prompt. Confirm both peers with
`repowire peer list`, then in `project-a`:

> Ask project-b what API endpoints they expose.

The agent calls the `ask` MCP tool. `project-b` receives the question and acks back with `ack(corr_id, "...")`. The reply lands in `project-a` framed as `[ack #cid from @project-b] ...`.

Repowire is not a standalone chat UI in this flow. You ask your local agent in natural language, and that agent invokes Repowire's MCP tools.

## What to read next

- [Start](start/index.md) walks through install, setup, and the first cross-repo ask.
- [Use](use/index.md) covers feature pages and workflow recipes for active users.
- [Concepts](concepts/index.md) covers peers, sessions, backends, transports, messages, jobs, and lazy repair.
- [Operate](operate/index.md) covers daemon, relay, transports, state, security, deployment, and architecture.
- [MCP tools reference](reference/mcp-tools.md) is the source of truth for the agent API.
- [CLI reference](reference/cli.md) covers setup, services, peers, schedules, bots, and diagnostics.
