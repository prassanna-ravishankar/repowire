---
name: feedback_live_verify_interaction
description: "Live-verified" means the interaction landed (click to recorded POST), not the render; SSE test stubs need a threaded server.
metadata:
  type: feedback
---

Do not claim something is live-verified until the actual interaction lands (click, then the POST is recorded), not just the render. If a stub or CORS artifact blocks verification, fix it and re-prove instead of narrating around it.

**Why:** A dashboard answer-banner click to `/answer` was claimed verified when the POST never landed (2026-05-31). The cause was a single-threaded `http.server` stub: the long-lived SSE connection (`/events/stream`) held its only thread, so POSTs were refused.

**How to apply:** Any hand-rolled stub that serves SSE plus other requests uses `ThreadingHTTPServer`. Related: [[feedback_live_testing]].
