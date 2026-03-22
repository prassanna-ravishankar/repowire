# Repowire Implementation Audit — 2026-03-22

Audit scope: brittleness in abstractions, overlapping concerns, and edge cases around tmux/peer lifecycle/recovery. All 222 tests pass; findings are structural.

---

## Live System Evidence (from hands-on testing)

Before diving into findings, here's what the live daemon reveals right now:

**1. Dual-registry divergence (confirmed):** Peer `2a59d87a` (`repow-some-session-40a27ef9`) — SessionMapper says `circle: some-session`, PeerManager says `circle: 0`. The tmux session was renamed from "some-session" to "0"; lazy_repair updated PeerManager but SessionMapper was not updated. The peer_id still contains "some-session" in its name.

**2. Ghost peer duplication (confirmed):** Display name `ffa8a5da` exists in TWO circles: `repow-2-d37e9661` (circle "2") and `repow-default-591bdf17` (circle "default"). Same session_id, same path. Ghost eviction didn't clean this up because both were in different circles and neither was OFFLINE at registration time.

**3. Orphaned ws-hook processes (confirmed):** 4 ws-hook processes running but only 1 has a live pane. The 3 orphans are **from pytest** — tests spawn real subprocess ws-hooks that outlive the test. They've been running for hours/days (`PID 20357` has been up for 2+ days). PID files don't track them because they use temp directories.

**4. Artifact accumulation:** 115 log files, 17 PID files, 16 of which point to DEAD processes. No cleanup mechanism exists. The `ws-hook-unknown.pid` file was created when a ws-hook ran without `TMUX_PANE` set.

**5. HTTP vs WebSocket field inconsistency:** HTTP peer registration requires `name` field. WebSocket connect uses `display_name`. The `Peer` model has `handle_legacy_fields()` to bridge this, but the API surface is inconsistent.

**6. Test pollution of live state:** `abc12345` test artifact peer (`repow-0-268f1caa`) exists in the live registry with path pointing to a pytest temp directory. Tests registered peers on the live daemon and didn't clean up.

---

## Executive Summary

The bugs you're seeing likely trace to **three root causes**:

1. **Dual-registry divergence** — PeerManager and SessionMapper both store circle/display_name/backend independently. They're updated together in the happy path, but any failure partway through leaves them inconsistent.

2. **Identity is fragile across tmux mutations** — display_name, pane_id, and circle are all derived from tmux state at hook time. Rename a window/session or move a pane, and the identity chain breaks silently.

3. **Response delivery assumes FIFO** — stop hook can't pass a correlation_id, so `resolve_oldest_query()` assumes responses arrive in order. User interrupts or multi-query scenarios break this.

---

## Finding 1: Dual-Registry State Drift (PeerManager × SessionMapper)

**Severity: HIGH — root cause of ghost peers and identity confusion**

Both `PeerManager._peers` and `SessionMapper._mappings` store circle, display_name, and backend. They're always updated together in current code, but:

- **No transactional guarantee.** If `register_peer()` succeeds but `register_session()` already returned a stale session_id, the two registries disagree on which peer_id maps to which identity.
- **No reconciliation.** There's no check that SessionMapper and PeerManager agree. A crash or exception between the two writes leaves permanent drift.
- **Session reuse across machines.** `SessionMapper.register_session()` matches on `(display_name, circle, backend)` and reuses the session_id. If a peer with the same folder name starts on a different machine, it hijacks the session_id, and responses from the old machine route to the new one.

**Concrete scenario:** Peer A registers as `frontend@dev`. Daemon restarts, SessionMapper loads from disk, PeerManager is empty. Peer B connects as `frontend@dev` — SessionMapper reuses A's session_id. If A's ws-hook is still running (orphaned process), both A and B share a peer_id.

**Files:** `session_mapper.py:96-107`, `core.py:146-168`, `routes/websocket.py:124-148`

**Recommendation:** SessionMapper should be the single source of identity allocation; PeerManager should only hold live state. Consider merging them or making PeerManager delegate identity lookups to SessionMapper.

---

## Finding 2: Tmux Rename Breaks Identity Chain

**Severity: HIGH — direct cause of "peer not found" after rename**

The identity chain is: tmux session name → circle, tmux pane → pane_id, claude session_id → display_name. Each is captured at hook invocation time and never refreshed except by `lazy_repair()` pong.

| Mutation | What breaks | Recovery |
|----------|------------|----------|
| `tmux rename-session dev2` | Circle changes from "dev" to "dev2" | `lazy_repair()` updates circle from pong (30s delay). Queries to "dev" circle fail in the interim. |
| `tmux rename-window backend` | Nothing — window name isn't used for identity | — |
| Pane destroyed + recreated | `pane_id` stale everywhere | ws-hook detects pane death on next ping → exits → daemon marks OFFLINE. But stop hook still references old pane_id → 404 on `/response`. |
| `tmux move-pane` | Pane ID may change (tmux gives new `%N`) | ws-hook still holds old pane_id → all tmux send-keys fail silently. Peer appears ONLINE but can't receive queries. |

**The 30s lazy_repair gap is the worst part.** A renamed session causes all queries to fail with "circle boundary" errors for up to 30 seconds. No error is surfaced to the user.

**Files:** `session_handler.py:159`, `websocket_hook.py:206`, `core.py:556-562`

**Recommendation:**
- `lazy_repair()` should run on query failure (not just on timer), providing immediate recovery.
- The pong should include pane_id so daemon can detect pane migration.
- Consider deriving circle from config rather than tmux session name (more stable).

---

## Finding 3: Response Delivery is Correlation-Blind

**Severity: HIGH — causes wrong query getting wrong answer**

The stop hook extracts the assistant's response from the transcript and POSTs to `/response` with just `pane_id`. The daemon then calls `query_tracker.resolve_oldest_query(peer_id, text)` — pure FIFO.

**Breaks when:**
- User sends query to peer A, then sends a second query before first response arrives. Stop hook fires once, resolves the oldest query. Second stop hook fires, but the transcript may contain a response to a different question.
- User interrupts Claude with Escape. The notification hook fires (BUSY→ONLINE), but stop hook also fires. Both try to update status; stop hook may deliver a partial response to the oldest query.
- Peer receives a notification while a query is pending. Stop hook can't distinguish between "response to query" and "response to notification".

**The fundamental issue:** stop hook doesn't know _which_ query it's responding to. It just delivers the last assistant turn.

**Files:** `stop_handler.py:74-75`, `query_tracker.py:165-194`, `routes/messages.py:220-240`

**Recommendation:** Pass correlation_id through the query injection text (e.g., as a `[repowire:corr_id=xxx]` tag) so the stop hook can extract it from the transcript and include it in `/response`. This makes response delivery deterministic.

---

## Finding 4: PID Dedup is a TOCTOU Race

**Severity: MEDIUM — causes duplicate ws-hooks or missed registrations**

Session handler checks if ws-hook is alive via PID file:

```python
old_pid = int(pid_path.read_text().strip())
os.kill(old_pid, 0)  # probe
return 0  # alive → skip
```

**Races:**
1. **PID recycled.** Between reading the PID and probing, the original process dies and a new unrelated process gets the same PID. The check succeeds, ws-hook is NOT spawned, peer becomes unreachable.
2. **File stale.** ws-hook crashes without cleaning up PID file. Next SessionStart reads stale PID, probes a random process, skips registration.
3. **Concurrent SessionStart.** Two tool sub-sessions fire SessionStart simultaneously. Both read the PID file, both see it's dead, both spawn a ws-hook. Two ws-hooks connect to daemon — second one replaces first in transport, but first is now orphaned.

**Impact:** In practice this manifests as "peer is online but queries timeout" (orphaned ws-hook) or "peer never registers" (skipped due to stale PID).

**Files:** `session_handler.py:121-129`

**Recommendation:** Use a filesystem lock (flock) around the PID check + spawn. Also validate the PID belongs to a Python process (check `/proc/{pid}/cmdline` on Linux or `ps -p` on macOS).

---

## Finding 5: Ghost Eviction Can't Evict Reused session_id

**Severity: MEDIUM — causes stale peer state**

Ghost eviction in `register_peer()`:
```python
if old_sid != peer.peer_id and (old_peer.circle == peer.circle or old_peer.status == OFFLINE):
    del self._peers[old_sid]
```

If SessionMapper reuses the same session_id (same display_name + circle + backend), `old_sid == peer.peer_id` is True → the old entry is NOT evicted. Instead, line 168 overwrites it: `self._peers[peer.peer_id] = peer`. This is mostly fine, but:

- Pending queries from the old peer's session are still tracked under the same peer_id in QueryTracker.
- The old peer's queries had different correlation_ids. When the new peer responds, `resolve_oldest_query()` resolves the old peer's query with the new peer's answer.

**Files:** `core.py:156-163`, `session_mapper.py:96-107`

**Recommendation:** On peer re-registration with same session_id, explicitly cancel all pending queries for that peer_id before inserting the new peer.

---

## Finding 6: QueryTracker Has No Lock (Safe by Accident)

**Severity: MEDIUM — time bomb**

QueryTracker uses no asyncio.Lock. The docstring claims "synchronous and run atomically within the asyncio event loop." This is true today — none of the methods contain `await`. But:

- If anyone adds an `await` inside `resolve_query()`, `register_query()`, or `cancel_queries_to_peer()`, the asyncio event loop can context-switch mid-operation.
- The two-dict invariant (`_queries` and `_by_peer_id` must stay in sync) would break.

**Files:** `query_tracker.py:78-84`

**Recommendation:** Add an asyncio.Lock and a comment explaining why. Defensive cost is near-zero; debugging a corruption from a future `await` would be expensive.

---

## Finding 7: Display Name Derived in 3 Places Independently

**Severity: MEDIUM — causes identity mismatch between hooks**

Display name is computed by:

| Location | Logic | Risk |
|----------|-------|------|
| `session_handler.py:132` | `claude_session_id[:8] or folder_name` | First hook to run |
| `stop_handler.py:40` | `claude_session_id[:8] or Path(cwd).name` | If session_id missing, uses cwd which may differ |
| `websocket_hook.py:212` | `REPOWIRE_DISPLAY_NAME` env var or cwd folder name | Env var set by session_handler |

If session_handler sets `display_name = "00893aaf"` and passes it via env var, ws-hook gets it right. But stop_handler re-derives it independently. If Claude's `session_id` is missing from stop hook input for any reason, it falls back to `Path(cwd).name` — which is the folder, not the 8-char hash. Now stop hook posts chat turns tagged to "myproject" while the peer is registered as "00893aaf".

**Files:** `session_handler.py:132`, `stop_handler.py:40`, `websocket_hook.py` + `utils.py:25`

**Recommendation:** Extract display_name derivation into a single shared function. Or better: have the daemon remember which pane_id maps to which display_name (it already does via `Peer.pane_id`), and have stop hook just send pane_id without display_name.

---

## Finding 8: add_event() Writes Without Lock

**Severity: LOW — benign in CPython, not spec-guaranteed**

```python
def add_event(self, event_type, data):
    self._events.append({...})  # No lock
    self._events_dirty = True   # No lock
```

`deque.append()` is thread-safe in CPython due to GIL, and this runs in asyncio (single-threaded). But `_save_events()` calls `list(self._events)` during `lazy_repair()`, and concurrent appends during serialization could produce a snapshot with partial state.

**Files:** `core.py:90-102`, `core.py:80-88`

**Recommendation:** Either acquire `self._lock` in `add_event()` (cheap) or acknowledge the risk in a comment.

---

## Finding 9: WebSocket Handler Swallows Exceptions in Loop

**Severity: LOW — causes persistent error loops**

```python
while True:
    data = await websocket.receive_json()
    try:
        await _handle_message(...)
    except Exception as e:
        logger.error(...)
        await websocket.send_json({"type": "error", ...})
```

If `_handle_message()` raises repeatedly (e.g., corrupted message state), the loop continues forever, logging errors and sending error responses. No backoff, no circuit breaker, no disconnect.

**Files:** `routes/websocket.py:155-176`

**Recommendation:** Add an error counter. After N consecutive errors (e.g., 5), close the WebSocket with an error code.

---

## Finding 10: Lazy Repair Circle Recovery Can Cause Cascading Failures

**Severity: LOW — but surprising behavior**

`lazy_repair()` updates a peer's circle if the pong reports a different tmux session name:

```python
if new_circle != current:
    await self.set_peer_circle(peer_id, new_circle)
```

`set_peer_circle()` acquires `self._lock`, but `_do_repair()` has already released it (line 523). Between releasing the lock and acquiring it again in `set_peer_circle()`, another coroutine could modify the peer (e.g., unregister it). The `set_peer_circle()` call would then log a warning but not crash.

More concerning: if a user quickly renames their tmux session back and forth, each `lazy_repair()` cycle updates the circle, potentially bouncing peers between circles and breaking in-flight queries.

**Files:** `core.py:556-562`, `core.py:437-450`

---

## Structural Recommendations

### 1. Merge SessionMapper into PeerManager (or vice versa)
The dual-registry is the single biggest source of brittleness. One component should own peer identity, state, and persistence. The other should be removed or reduced to a thin cache.

### 2. Make correlation_id flow end-to-end
The stop hook → `/response` path should carry a correlation_id. Without it, response routing is a best-guess FIFO. This is the root cause of "wrong answer to wrong question" bugs.

### 3. Add a "peer health check on query failure" path
Currently `lazy_repair()` only runs on a 30s timer. If a query fails because the target peer is actually dead, the caller waits the full 300s timeout. Running a targeted liveness check on the specific peer before/after query failure would surface problems immediately.

### 4. Centralize display_name derivation
Three independent derivation paths is asking for divergence. One function, one source of truth.

### 5. Replace PID file dedup with flock
The current TOCTOU race in session_handler is a known pattern. `flock` on a per-pane lock file eliminates the race entirely.

---

## Finding 11: Tests Leak Real Processes and Pollute Live State

**Severity: MEDIUM — actively harming your dev environment**

Tests that exercise `session_handler.main()` spawn real `subprocess.Popen` ws-hook processes. These:

1. **Outlive the test.** `start_new_session=True` daemonizes them. pytest doesn't kill them. Three orphan ws-hooks from tests are running right now (PIDs 20357, 36185, 36186), one for **2+ days**.
2. **Pollute the live daemon.** Tests register peers on `127.0.0.1:8377` (the real daemon). `abc12345` test peer is in the live registry pointing at a pytest temp dir.
3. **Leave PID files behind.** PID files in temp dirs are cleaned up, but the processes they spawned are not.

**Files:** `tests/test_session_handler.py` (no process cleanup in teardown)

**Recommendation:** Tests should mock `subprocess.Popen` or use a fixture that kills spawned processes on teardown. Tests that call HTTP endpoints should use the ASGI test client, not the live daemon.

---

## Finding 12: No Artifact Cleanup Mechanism

**Severity: LOW — but accumulates over time**

115 ws-hook log files and 17 PID files (16 stale) sit in `~/.cache/repowire/logs/`. No expiry, no rotation, no cleanup on daemon start. SessionMapper accumulates entries forever (23 entries currently, most OFFLINE from days ago).

**Recommendation:** Daemon startup should prune PID files for dead processes and log files older than N days. `lazy_repair()` could evict OFFLINE peers not seen for >24h (current 72h eviction threshold is too generous).

---

## Test Coverage Gaps

Current: 222 tests, all passing. Notable gaps:

| Scenario | Tested? |
|----------|---------|
| Fast WS disconnect + reconnect (same peer) | No |
| Session rename mid-query | No |
| Pane move (`tmux move-pane`) | No |
| Two peers with same display_name, different circles, concurrent query | No |
| Stop hook fires without pending query | Partially (unit test only) |
| PID file points to recycled PID | No |
| SessionMapper + PeerManager divergence after partial failure | No |
| `resolve_oldest_query()` with out-of-order responses | No |
| Circle change during in-flight query | No |
| ws-hook reconnect after daemon restart | No |
| Test process cleanup (ws-hooks spawned by tests) | No |
| Ambiguous display_name lookup returns correct peer | No |
