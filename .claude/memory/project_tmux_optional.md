---
name: tmux-must-stay-optional
description: Tmux is optional — architecture must work without it, may be dropped entirely
type: project
---

Tmux support must remain a clean, removable adapter layer. The daemon, peer registry, and message router must never import or reference tmux directly.

**Why:** Prass explicitly stated "tomorrow we might want no tmux support" — tmux is a convenience transport, not a core dependency. The system must degrade gracefully without it.

**How to apply:** Any tmux-related code belongs in `repowire/hooks/` (client-side) or behind a `LifecycleEventHandler` protocol (daemon-side). Never add tmux imports to `daemon/peer_registry.py`, `daemon/message_router.py`, or `daemon/query_tracker.py`. Use runtime detection (`shutil.which("tmux")`) and silent fallback.
