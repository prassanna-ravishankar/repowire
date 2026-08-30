# vs Happy

## What it is

[Happy](https://happy.engineering/) is a mobile and web client for Claude Code and Codex, with bidirectional realtime sync between the desktop CLI and the mobile app, end-to-end encrypted relay, voice input, and push notifications. The CLI wraps Claude Code or Codex execution, streams terminal state to the mobile app, and feeds mobile input back into the same shared session.

## Workflow boundary

Happy's job is **giving a single coding session a phone**. You start a Claude Code session at your desk; Happy lets you continue that exact session from your phone, with the same context, the same scrollback, the same approvals.

It does not coordinate multiple agents. One CLI process = one Happy session = one mobile thread.

## Architecture difference

- **Happy** wraps a single agent CLI and proxies its terminal state. A relay sits between the CLI and the mobile/web client; end-to-end encryption means even the relay can't read the session.
- **Repowire** runs as an out-of-process daemon and routes addressed messages between many concurrent agent sessions. The daemon is the single source of truth for peer state.

Different shapes: Happy is a transport for one session's I/O; repowire is a router for many sessions' messages.

## When to use repowire instead

- You have more than one agent session running and want them to talk to each other.
- You want cross-runtime work (Claude Code asking Codex; Pi reviewing OpenCode).
- You need a control surface that aggregates state across peers (dashboard, Telegram, Slack as peers, not just as one session's terminal).

## When to use Happy instead

- You want to keep working in one session from your phone while you're away from your desk, with full context and approvals.
- You need end-to-end encryption between the CLI and the client. Repowire's relay tunnels are TLS to the relay, not end-to-end.
- You don't have a multi-agent workflow yet — single-session mobility is what you need.

## Can you use both?

Yes, and they compose cleanly: Happy gives one of your sessions a phone interface, while repowire links that same session to other agent sessions on your machine. The repowire Telegram bot covers a different surface — driving the *mesh* from your phone rather than wrapping one session. Use Happy when you want to *be* the agent from your phone; use the repowire Telegram bot when you want to *direct* agents from your phone.
