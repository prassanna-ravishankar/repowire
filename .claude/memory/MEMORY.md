# Repowire Project Memory

<!-- Index of memory files — one line per entry, under 150 chars each -->

- [feedback_live_testing.md](feedback_live_testing.md) - Live test installer changes on a real machine before merging; unit tests alone have missed regressions.
- [project_notification_interruption.md](project_notification_interruption.md) - Bare Escape in `_tmux_send_keys` cancels mid-tool-call TUI actions; use hex `ESC[201~` instead.
- [Tmux must stay optional](project_tmux_optional.md) — tmux is a removable adapter, never a core dependency; may be dropped entirely
- [Lifecycle hooks architecture](project_lifecycle_hooks.md) — provider-agnostic /hooks/lifecycle/* endpoints, tmux hooks via set-hook -g
- [Pane takeover semantics](project_pane_takeover.md) — same-pane restarts are fresh takeovers; clear pane state and dedupe Codex tmux registration
