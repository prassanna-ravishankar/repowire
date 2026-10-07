---
name: cross-agent-plan
description: Request an independent implementation plan or design critique from a local coding agent through Repowire remote MCP.
---

Use `list_machines` and `list_agents` to find an existing agent with the relevant
project context. Honor the user's backend choice; otherwise prefer a different
backend for an independent perspective. Clarify ambiguous project matches.

Use `ask_agent` with exact machine and peer IDs. Supply the goal, constraints,
relevant design context, and desired tradeoffs or validation. Explicitly request
planning only; this request does not authorize implementation.

Save the returned request ID. Retrieve the plan with `get_reply`, optionally
waiting up to 20 seconds. Pending means work is still running. Evaluate the
returned plan and identify unresolved choices; do not silently convert it into
permission to execute. Include relevant prior context in any follow-up ask.

If sending times out, inspect outstanding work with `list_requests` before
considering a retry. The remote tools cannot create a missing planner session.
