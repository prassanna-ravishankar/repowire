---
name: pane-takeover-semantics
description: Same-pane restarts are fresh takeovers; pane-scoped state is transient and Codex tmux registration must dedupe to one peer
type: project
---

For tmux-backed sessions, a new real agent process in the same pane is treated as a fresh takeover by default. Only repeated `SessionStart` events with the same live hook session identity count as ephemeral sub-sessions and should skip ws-hook replacement.

Pane-scoped runtime state in `repowire/hooks/utils.py` is transient. Pending correlation IDs and ws-hook metadata must be cleared on pane takeover, `pane-died`, and `session-closed` handling so old replies cannot attach to a new logical session.

The ws-hook in `repowire/hooks/websocket_hook.py` should retire itself when the pane is no longer running the expected agent command. Do not leave a connected peer online just because the tmux pane still exists as a shell.

Codex MCP lazy registration in `repowire/mcp/server.py` must resolve through `pane_id` and the tmux session circle when running inside tmux. Do not create a second default-circle peer for the same live pane-backed session.

**Why:** These rules prevent stale online peers, pane-reuse reply misrouting, broken backend swaps, and duplicate Codex identities.

**How to apply:** Changes touching pane ownership should keep takeover logic in `repowire/hooks/session_handler.py`, pane state helpers in `repowire/hooks/utils.py`, and daemon-side cleanup in `repowire/daemon/lifecycle_handler.py` or `repowire/daemon/peer_registry.py`.
