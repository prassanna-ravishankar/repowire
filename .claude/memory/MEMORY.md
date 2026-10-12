# Repowire Project Memory

<!-- Index of memory files — one line per entry, under 150 chars each -->

- [feedback_live_testing.md](feedback_live_testing.md) - Live test installer changes on a real machine before merging; unit tests alone have missed regressions.
- [project_notification_interruption.md](project_notification_interruption.md) - Bare Escape in `_tmux_send_keys` cancels mid-tool-call TUI actions; use hex `ESC[201~` instead.
- [Tmux must stay optional](project_tmux_optional.md) — tmux is a removable adapter, never a core dependency; may be dropped entirely
- [Lifecycle hooks architecture](project_lifecycle_hooks.md) — provider-agnostic /hooks/lifecycle/* endpoints, tmux hooks via set-hook -g
- [Pane takeover semantics](project_pane_takeover.md) — same-pane restarts are fresh takeovers; clear pane state and dedupe Codex tmux registration
- [Telegram brevity](feedback_telegram_brevity.md) — notify_peer messages to the human must be short/phone-readable; lead with the point, detail goes in PR/vault/issue
- [Docs IA plan](project_docs_ia_plan.md) — docs/ re-arrange: 6 grouped lanes (Use/Operate/Support), guides+capabilities→Features, patterns→Workflows; shipped in #348
- [Check branch before commit](feedback_check_branch_before_commit.md) — when a peer owns a shared branch, verify `git branch --show-current` before committing local edits
- [Live-test round-trips](feedback_live_test_roundtrip.md) — don't block polling /events during a mesh round-trip; end the call and let the peer message arrive
- [Live-verify the interaction](feedback_live_verify_interaction.md) — verified means the POST landed, not the render; SSE stubs need ThreadingHTTPServer
- [Pull before push](feedback_git_pull_before_push.md) — always `git pull --rebase` before any push; peers share branches
- [Read linked design docs](feedback_read_linked_design_docs.md) — read the source-of-truth design doc a handoff links, not just the summary
