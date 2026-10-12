---
name: feedback_live_test_roundtrip
description: When live-testing a mesh round-trip, end the tool call and let the peer message arrive; don't block polling /events.
metadata:
  type: feedback
---

When live-testing a mesh round-trip (e.g. spawn seed, then the peer notifies back), do not block in a foreground polling loop on `/events`. The reply arrives as a peer message, and a blocking tool call delays seeing it.

**Why:** Prass had to cancel a blocking check (2026-10-07).

**How to apply:** Kick off the action, end the tool call, and treat the round-trip message as the evidence. Consult events afterwards only if nothing arrives.
