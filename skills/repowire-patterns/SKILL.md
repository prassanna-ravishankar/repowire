---
name: repowire-patterns
description: Reference for how to use the repowire mesh — ask/ack vs notify, broadcast, peer discovery, spawning, and cross-agent workflows. Use when you need to coordinate with other AI agents over repowire and want the right primitive for the job.
---

# Repowire usage patterns

Repowire is a mesh where AI coding agents (Claude Code, Codex, Gemini, OpenCode,
Pi, …) each get an address and talk to each other. This is a teaching reference;
the action skills (`cross-agent-review`, `cross-agent-plan`, `delegate`) apply
these patterns. It does not depend on those skills being installed.

## Pick the right primitive

| Want | Use | Lifecycle |
|------|-----|-----------|
| Tracked request that needs a reply | `ask(peer, text)` → peer `ack(corr_id, reply)` | Non-blocking; returns `correlation_id`; reopen with `ask(reply_to=...)` |
| Fire-and-forget nudge / status | `notify_peer(peer, text)` | No reply expected |
| Message everyone in your circle | `broadcast(text)` | No per-peer lifecycle |
| Close an inbound ask | `ack(corr_id)` bare, or `ack(corr_id, reply)` | The only close/reply op for an ask |
| See who's around | `list_peers()` | Returns names, backends, status |
| Who am I | `whoami()` | Your peer identity/circle |

The tracked **ask/ack/notify/broadcast lifecycle is MCP-only** — use the
`mcp__repowire__*` tools. There is no honest CLI equivalent for non-blocking ask
(the `repowire peer ask` command is a synchronous testing utility, not the
ask/ack lifecycle). For agents without MCP, the CLI offers these real fallbacks:
`repowire peer list`, `repowire peer whoami`, `repowire peer ack <cid>`,
`repowire peer asks` (list pending), `repowire peer new` (spawn).

## Rules that bite

- **`ask` is non-blocking** — it returns a `correlation_id`, not a reply. The reply
  arrives later as an `ack`. Don't wait synchronously.
- **`ack` is the only way to close an inbound ask.** Bare `ack(corr_id)` = "seen,
  no action"; `ack(corr_id, msg)` = reply. Unacked asks resurface as reminders.
- **Identity is `peer_id`; addressing is `display_name`.** Names can collide; pass
  `circle` to disambiguate.
- **Cross-agent means a different backend.** For an independent review/plan, target
  a peer whose backend differs from yours (see `cross-agent-review`/`cross-agent-plan`).
- **Spawn deliberately.** `spawn_peer` starts a new agent; confirm with the user
  rather than spawning silently.

## Cross-agent workflows

- Independent review: `cross-agent-review` (have a different backend review your work).
- Independent planning: `cross-agent-plan`.
- Offload work: `delegate` (reuse or spawn a peer, hand off, track via ack).

Backends for these are parameterised via `repowire config get skills.*` defaults,
overridable per call — never hardcode a backend.
