---
name: cross-agent-review
description: Get a code/PR/plan review from a DIFFERENT AI agent over the repowire mesh (e.g. have Codex review Claude's work). Use when you want an independent second opinion from another backend before merging or committing.
---

# Cross-agent review

Ask a peer running a **different** agent backend to review your work, so the
review is genuinely independent (not the same model checking itself).

## Resolve the reviewer backend

Pick the reviewer in this order:

1. **Explicit argument** — if the user named a backend (e.g. "review with codex"), use it.
2. **Configured default** — else read it:
   ```bash
   repowire config get skills.default_reviewer_backend
   ```
   (Prints the value, or nothing if unset. `--json` for the raw value.)
3. **Safe fallback** — if still unset, pick any *online peer whose backend differs
   from yours*, or ask the user which backend to use. **Never** default to your own
   backend — that defeats the purpose. Never hardcode a specific backend.

You can see who is available and on which backend:
- MCP: `list_peers()`
- CLI: `repowire peer list`

## Run the review

1. Find (or spawn) a peer on the chosen backend.
2. Send it a review request with the concrete diff/PR/plan to review. Be specific
   about what to check.
   - MCP: `ask(peer_name, "Review this diff for correctness + bugs: <context>")`
   - The tracked ask/ack lifecycle is MCP-only — there's no CLI equivalent for a
     non-blocking ask (`repowire peer ask` is a synchronous test utility, not this).
3. `ask` is non-blocking and returns a `correlation_id`. The reviewer closes the
   thread with `ack(corr_id, <their review>)` — that reply comes back to you.
4. Apply the review; iterate with `ask(reply_to=corr_id, ...)` for follow-ups.

If no different-backend peer is available and the user wants one, spawn it
(see the `delegate` skill) — but only with the user's go-ahead.
