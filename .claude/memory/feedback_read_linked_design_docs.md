---
name: feedback_read_linked_design_docs
description: When a handoff links a source-of-truth design doc, read the design doc itself before implementing.
metadata:
  type: feedback
---

When a handoff doc links a source-of-truth design doc, read the linked design doc directly before implementing. Do not rely on the handoff's summary, or on a peer (even the doc's co-author), as a proxy.

**Why:** Prass callout 2026-05-31 (beads `repowire-qnp`). Design docs also record decisions that were never built (e.g. `Question.recipient` routing metadata, see #439), which a handoff summary can silently omit.

**How to apply:** Open the linked doc first; reconcile it against what actually shipped.
