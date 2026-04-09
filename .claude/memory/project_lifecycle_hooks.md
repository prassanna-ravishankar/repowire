---
name: lifecycle-hooks-architecture
description: Provider-agnostic lifecycle event system — tmux hooks POST to daemon, handler updates PeerRegistry
type: project
---

Lifecycle events are provider-agnostic HTTP endpoints at `/hooks/lifecycle/*` (pane-died, session-closed, session-renamed, window-renamed, client-detached). The daemon doesn't know or care what fires them.

Tmux hooks use numeric array index `[42]` to avoid clobbering user hooks at `[0]`. Session-level hooks use `-g`, window-level use `-gw`. `pane-exited` (not `pane-died`) for pane exit. Rename hooks use `after-rename-*` with pane ID collection since tmux doesn't provide the old name. Installed at daemon startup and `repowire setup`, cleaned up on `repowire uninstall`.

`lazy_repair` no longer polls — it only does debounced stale eviction + persistence. `active_repair` retains the ping/pong sweep for diagnostics.

**Why:** Replaced polling with reactive events. Aligns with "nothing polls" philosophy. Tmux hooks give instant detection of pane death, session close, and renames.

**How to apply:** All tmux process calls live in `repowire/hooks/_tmux.py`. Lifecycle handler in `repowire/daemon/lifecycle_handler.py` has no tmux imports. New tmux-related features should follow this split.
