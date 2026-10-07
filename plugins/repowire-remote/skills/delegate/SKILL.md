---
name: delegate
description: Delegate a user-requested task to an existing local coding agent through Repowire remote MCP and retrieve its completion report.
---

Discover the user's machines and agents with `list_machines` and `list_agents`.
Honor the requested project and backend; clarify ambiguous recipients. Use the
returned machine ID and peer ID, never an inferred display name.

Send `ask_agent` a self-contained brief: goal, relevant context, constraints,
allowed changes, and what evidence the completion report should contain. Do not
broaden authorization to deployment, publication, or unrelated work.

Save `daemon_id` and `request_id`, then retrieve the report with `get_reply`,
optionally waiting up to 20 seconds per call. Delivery acceptance is not task
completion. Pending work remains active; do useful independent work or report
that it is still running. Inspect the close reason and evidence before calling
it done. Further tasks use a new ask with the earlier context included.

For an uncertain send, inspect `list_requests` rather than automatically resending.
It lists only outstanding work; an absent request may already have completed.
If no suitable agent is connected, explain that the remote tools cannot spawn one.
