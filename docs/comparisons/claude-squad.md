# vs Claude Squad

## What it is

[Claude Squad](https://github.com/smtg-ai/claude-squad) is a terminal app for managing multiple local AI coding agents — Claude Code, Codex, Gemini, Aider — in isolated workspaces. It uses tmux for session isolation and git worktrees for branch isolation, with a TUI on top. Each agent gets its own tmux pane and its own worktree.

## Workflow boundary

Claude Squad's job is **launching and switching between multiple isolated agent sessions** on one machine. It owns the *session lifecycle* (start, attach, kill) and the *workspace isolation* (worktree per session).

Repowire's job is **letting those sessions message each other** once they're running. It does not start or kill sessions for you (except via `spawn_peer`, which is a thin wrapper); its core is the routing layer between them.

## Architecture difference

- **Claude Squad.** Session manager. Owns tmux + worktree state. No inter-session communication path; each session is independent.
- **Repowire.** Routing daemon. Sits alongside whatever started the sessions. Adds a WebSocket layer between agent runtimes, control surfaces, and a dashboard.

Different layers of the stack. Claude Squad answers "how do I run five agents in parallel?" Repowire answers "how do five running agents talk to each other?"

## When to use repowire instead

- The sessions need to coordinate, ask each other questions, or hand off work.
- You want a dashboard or mobile surface that aggregates state across all sessions.
- You're mixing local agents with control surfaces (Telegram, Slack) or remote dashboards.

## When to use Claude Squad instead

- You want a polished TUI to launch and switch between isolated agent sessions.
- You don't need inter-session communication — each agent is doing its own task.
- Your workflow is fundamentally parallel-and-independent, not collaborative.

## Can you use both?

Yes, and they pair naturally. Claude Squad launches your isolated sessions in tmux + worktrees; repowire's hooks fire on each session's `SessionStart` and register it as a peer. From then on, sessions Claude Squad started can `ask` each other through repowire's routing layer.

Conceptually: Claude Squad is the launcher and isolator; repowire is the switchboard. They sit at different layers and don't collide.
