# Codex

OpenAI's Codex CLI. Hooks and the MCP server are wired into `~/.codex/`.

## What gets installed

| File | What goes there |
| --- | --- |
| `~/.codex/hooks.json` | Hook entries for `SessionStart`, `UserPromptSubmit`, and `Stop` |
| `~/.codex/config.toml` | `[features] hooks = true` flag and a `[mcp_servers.repowire]` section |

The MCP entry points at the installed `repowire` binary:

```toml
[mcp_servers.repowire]
command = "repowire"
args = ["mcp"]
env = { REPOWIRE_BACKEND = "codex" }
```

## The hooks feature flag

Codex hooks default to **off**. Repowire writes:

```toml
[features]
hooks = true
```

into `config.toml`. If it finds the old `codex_hooks` flag, repowire removes
that deprecated key and preserves or writes the current `hooks` flag so Codex
does not emit a startup warning.

!!! note "Naming"
    Codex 0.129.0 renamed `codex_hooks` to `hooks`. Current Codex releases warn
    when the legacy key is present, so `repowire setup` migrates it away.

## Late SessionStart

Codex fires `SessionStart` **after the first user interaction**, not at startup. The repowire MCP server compensates for this with lazy registration — every MCP tool call runs `_ensure_registered()`, which idempotently registers the peer if `SessionStart` hasn't fired yet. The practical consequence: a Codex peer may not appear in `list_peers` until you've typed something into it.

This is why `spawn_peer(..., message=...)` is effectively required for Codex peers and optional for the others. The `message` becomes the seed first turn that fires `SessionStart`. If you call `spawn_peer` without a `message` for a Codex peer, repowire substitutes a short default warmup prompt to make the hook fire.

## Status Lifecycle

`UserPromptSubmit` marks the peer `busy` with `turn_state=working`. `Stop` marks it `online` with `turn_state=idle`.

Manual interrupt/Esc in the Codex TUI currently has no clean Repowire-visible lifecycle signal. If the interrupt aborts the visible turn without a `Stop` hook, the peer can stay `busy` until the next successful turn boundary or until the daemon's conservative stale-state repair sees `busy`/`working` with no recent liveness for longer than `daemon.stale_busy_timeout_seconds`. Repowire intentionally does not guess that a recent busy Codex peer is idle; when Codex exposes a reliable interrupt/cancel hook, Repowire should wire that directly.

Codex can also drop the background WebSocket hook while the TUI pane remains alive. Repowire treats that as transport loss, not proof that the runtime is dead: status stays runtime-driven when pane/process evidence still exists, but inbound ask/notify delivery fails loudly until the WebSocket hook reconnects.

## Verifying

```bash
repowire status
```

To confirm hooks fire, open a Codex session, type one message, and watch `repowire peer list`. The peer should appear within a few seconds of the first user message — not at session open.

## Troubleshooting

- Codex peer never registers → confirm `[features] hooks = true` is in `config.toml`, then see [Hooks not firing](../../troubleshooting/hooks.md).
- Codex peer shows up but `turn_state=pending_first_turn` → the spawn `message` never reached the agent. Re-send via `notify_peer`.
- MCP tools returning errors → check `~/.codex/config.toml` has the `[mcp_servers.repowire]` section, `env = { REPOWIRE_BACKEND = "codex" }`, and `repowire` is on `PATH`.
