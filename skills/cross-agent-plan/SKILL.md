---
name: cross-agent-plan
description: Get an independent implementation plan from a DIFFERENT AI agent over the repowire mesh before you build (e.g. have Gemini or Codex draft an approach for Claude to critique). Use when you want a second perspective on how to approach a task.
---

# Cross-agent plan

Ask a peer running a **different** agent backend to draft (or critique) a plan,
so the planning perspective is genuinely independent.

## Resolve the planner backend

1. **Explicit argument** — if the user named a backend, use it.
2. **Configured default** — else:
   ```bash
   repowire config get skills.default_planner_backend
   ```
   (Empty output means unset.)
3. **Safe fallback** — else pick an online peer on a backend *different from yours*,
   or ask the user. Never default to your own backend; never hardcode one.

Discover peers/backends: `list_peers()` (MCP) or `repowire list-peers` (CLI).

## Run the planning round

1. Find (or, with the user's go-ahead, spawn — see `delegate`) a peer on the
   chosen backend.
2. Send the task + constraints and ask for a concrete step-by-step plan.
   - MCP: `ask(peer_name, "Draft an implementation plan for: <task + constraints>")`
   - CLI: `repowire ask <peer> "..."`
3. `ask` returns a `correlation_id`; the peer replies via `ack(corr_id, <plan>)`.
4. Critique/merge the returned plan with your own; chain follow-ups with
   `ask(reply_to=corr_id, ...)`.

Keep the brief tight — state the goal, the constraints, and what a good plan must
cover, so the cross-agent plan is comparable to your own.
