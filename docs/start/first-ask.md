# First ask

Open two agents in separate tmux windows. Claude Code registers through its
session hook; Codex registers through App Server as soon as its thread opens.

```bash
# window 1
cd ~/projects/project-a && claude

# window 2
cd ~/projects/project-b && codex
```

Before prompting either agent, verify both peers:

```bash
repowire peer list
```

In `project-a`, tell your local agent:

> Ask project-b what API endpoints they expose.

The agent calls Repowire's `ask` MCP tool with `peer_name="project-b"`. Repowire returns a `correlation_id` immediately — the call does not block. Repowire is the mesh/tool surface here; the chat still happens through your agent session.

`project-b` receives the question framed as `[ask #cid from @project-a] ...` and decides how to respond. When it closes the thread with `ack(cid, "...")`, the reply lands back in `project-a` as a notification framed `[ack #cid from @project-b] ...`.

Both halves work the same regardless of which supported runtimes you mix. Claude Code asking Codex looks identical to OpenCode asking Pi.

## What just happened

1. Claude Code's `SessionStart` hook and Codex's native thread event registered them as peers with the local daemon.
2. The daemon assigned each a display name (`project-a`, `project-b`) and put them in the same circle (the shared tmux boundary: a session by default, or a window when configured).
3. The MCP `ask` tool sent the message over HTTP to the daemon, which routed it to `project-b` via the chosen native transport: Codex thread steering, Claude's authenticated inbox, the OpenCode plugin, or the Pi extension.
4. The `ack` came back through the same path.

## Try a notification

```text
Tell project-b that the database migration is done.
```

The agent calls `notify_peer("project-b", "...")`. No correlation, no reply expected.

## Next

- [Concepts](../concepts/index.md) — peers, circles, message types, lazy repair.
- [MCP tools](../reference/mcp-tools.md) — the full agent surface.
- [Use](../use/index.md) — drive the mesh from features and workflow recipes.
