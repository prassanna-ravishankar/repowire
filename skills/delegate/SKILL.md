---
name: delegate
description: Hand a task to another AI agent peer over the repowire mesh — reuse an existing peer on the chosen backend, or spawn one if none exists. Use when you want to offload work to a specific agent (e.g. delegate a refactor to a Codex peer) and track it to completion.
---

# Delegate a task to a peer

Hand a self-contained task to a peer on a chosen backend and track it via the
ask/ack lifecycle.

## Resolve the delegate backend

1. **Explicit argument** — if the user named a backend, use it.
2. **Configured default** — else:
   ```bash
   repowire config get skills.default_delegate_backend
   ```
3. **Safe fallback** — else ask the user which backend to delegate to. Don't
   hardcode one. (Unlike review/plan, delegation MAY target your own backend —
   delegating to another claude-code peer is legitimate.)

## Choose / create the executor

1. **Prefer an existing peer** on the chosen backend (idle if possible):
   - MCP: `list_peers()` → pick an online peer with the right backend.
   - CLI: `repowire list-peers`.
2. **Spawn only if none exists AND the user is clearly delegating** (don't spawn
   silently). Confirm before spawning:
   - MCP: `spawn_peer(path, backend=<chosen>, circle=<skills.default_circle or current>)`
   - CLI: `repowire peer spawn ...`
   Default circle: `repowire config get skills.default_circle` (else your current circle).

## Hand off + track

1. Send a complete, self-contained brief (the peer doesn't share your context):
   - MCP: `ask(peer_name, "<full task brief + acceptance criteria>")`
   - CLI: `repowire ask <peer> "..."`
2. `ask` returns a `correlation_id`; the peer reports back via `ack(corr_id, ...)`.
3. Track to completion; chain follow-ups with `ask(reply_to=corr_id, ...)`.

Use `notify_peer` (fire-and-forget) only for nudges, not for tracked delegation —
delegation needs the ack lifecycle.
