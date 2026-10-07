---
name: cross-agent-review
description: Ask an existing local coding agent for an independent code, PR, or design review through Repowire remote MCP.
---

Discover machines and agents with `list_machines` and `list_agents`. Honor the
user's chosen reviewer. Otherwise prefer a suitable agent on a different backend
for a second perspective; clarify if no suitable reviewer is available.

Use `ask_agent` with exact daemon and peer IDs. Provide the PR URL, commit or
branch, concrete diff, or design text that needs review, plus the questions and
constraints. The agent cannot see this chat. Request findings and evidence;
a review request does not authorize edits, commits, or merges.

Save the returned request ID and use `get_reply` with a bounded wait to retrieve
findings. A pending result is not a completed review; a closed request without
findings is not approval. Assess the findings against the user's task, and ask
focused follow-ups as new requests with enough prior context.

After a send timeout, inspect `list_requests`; do not automatically send the
review twice. The remote tools cannot spawn a reviewer or read local files directly.
