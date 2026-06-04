# Whole-repo architecture scan (2026-06)

Status: architecture audit. Findings and prioritized refactor seams, not an
implementation commitment. Where this overlaps existing plans it defers to and
extends them rather than restating them.

This is a top-down pass over the entire tree (~46k LOC Python + the TypeScript
installers/channel), aimed at one question from the session-native roadmap:
**what structural changes set us up to add features faster?** The lens
throughout is the roadmap's stated direction — a *shared command surface*
(send / switch-backend / resume / schedule / approve) **reused from dashboard,
MCP, Telegram, and other surfaces**, transport-neutral routing, and sessions as
the durable unit of work.

## TL;DR

The foundations are sound: the transport-router seam, a real `AgentBackend`
capability registry, lazy-repair discipline, and schema-versioned SQLite are all
the right shapes. The problems are mostly **one problem repeated**: business
logic has settled in the layer that grew it, and a few hub objects have absorbed
responsibilities that want to be sliced out. None of this blocks today's work;
all of it taxes tomorrow's.

The single highest-leverage move is to finish promoting daemon logic into an
**application-service layer** that all surfaces (routes, MCP, CLI) call — which
is exactly what `registry-route-split-plan.md` started for the daemon-internal
split, but extended outward so MCP and the CLI stop re-implementing flows. That
is the concrete enabler for the roadmap's "shared command surface."

## What has already landed (credit where due)

This scan confirms the existing plans are on the right track and partially
executed:

- **`registry-route-split-plan.md` PR1 (EventLog) and PR3 (PeerDeliveryService)
  have shipped.** `daemon/event_log.py` and `daemon/peer_delivery.py` exist;
  `PeerRegistry` now delegates to them through compatibility shims
  (`_compat_delivery_service()` at `peer_registry.py:1538`; event property
  forwarders at `peer_registry.py:421-449`). This is real progress and the
  pattern is correct.
- **`sqlite-state-expansion-plan.md` is largely realized.** Schedules, session
  bindings, runtime identity certificates, dashboard/session events, and peer
  session mappings are on `~/.repowire/state.db` with one-time legacy JSON
  import. The deliberate separation of peer mappings from session bindings, and
  the choice to keep asks in-memory, are sound and should be preserved.

Two PRs from the registry/route plan remain open and are still the right next
steps: **PR2 (`peer_access.py` access resolver)** and **PR4 (`repair.py`
coordinator)**. This scan does not duplicate that plan; it adds the
cross-surface, backend-extensibility, and config-slicing dimensions that plan
does not cover.

## Finding 1 — The service layer is half-built; finish it outward (highest leverage)

`registry-route-split-plan.md` is extracting services *underneath the routes*
(EventLog, PeerDeliveryService, and the pending peer_access/repair). The gap this
scan adds: **the services don't yet front the user-facing flows, so every
surface re-implements them.**

Business logic still lives in the route handlers. `routes/peers.py` (1677),
`routes/spawn.py` (1345), `routes/asks.py` (1316) are not thin controllers — they
hold the ask/answer state machine, spawn orchestration, pane-ownership proof, and
ACP permission relay. Representative spots:

- `routes/asks.py:_answer_question_core` (~100 lines of answer/ack/approval state
  machine); `routes/asks.py:_acp_complete` (~116 lines of ACP relay + retry).
- `routes/spawn.py:restart_peer` (~195 lines of restart + resume + pane-probe
  orchestration). The route module still carries legacy compatibility helpers
  such as `_resolve_spawn_command` / `_validate_spawn_path`, although the
  command/path validation path has started moving into `SpawnService`.
- `routes/peers.py:_link_pane` (~123 lines); health computed inline in
  `_peer_to_info_with_health`.

Because the logic is reachable only behind HTTP, the other surfaces each rebuild
the flow:

| Flow | CLI (`cli.py`) | MCP (`mcp/server.py`) | HTTP route |
|------|-----|-----|-----|
| ask | builds raw dict → legacy `/query` | builds payload → `/ask` | full logic in `open_ask` |
| ack | `peer_ack` | `ack` tool | `ack_ask` |
| list_peers | inline `httpx.Client()` | `list_peers` tool | `list_peers` |

Notably the **CLI does not use the project's own `client.py`** (727 LOC, 44 typed
async methods, proper error hierarchy) — it constructs `httpx.Client()` inline
across dozens of commands, and CLI's ask targets the legacy `/query` endpoint
while MCP's ask targets `/ask`. Three surfaces, three code paths, drifting
semantics.

This is the direct blocker for the roadmap's shared command surface: you cannot
reuse send / switch-backend / resume / approve across dashboard + MCP + Telegram
while each control's logic is welded inside a FastAPI handler.

**Direction.** Continue the registry/route plan, but make the extracted units
*application services* with no HTTP knowledge: `AskService`, `QuestionService`,
and a promoted `SpawnService` and `PeerService`. Routes become parse → call
service → serialize. MCP tools and CLI call the **same** services (in-process in
the daemon; over `client.py` when remote). The DI seam already exists
(`deps.py:get_app_state()` / `get_peer_registry()`), so this proceeds one flow at
a time. **Start with ask/ack** — smallest cut that proves route + MCP + CLI all
calling one service end-to-end.

Adjacent low-risk win: give the CLI a sync wrapper over `client.py` and delete the
inline `httpx` calls; this collapses the `/query` vs `/ask` divergence on its own.

## Finding 2 — `peer_registry.py` is still a god-object (2796 LOC, 94 registry methods)

Even after the EventLog/delivery extractions, the registry is six classes in a
trenchcoat: in-memory peer store, session-mapping persistence (hand-written SQL
inline at `peer_registry.py:315-360`, unlike every other domain which has a clean
`state/*Store`), repair/reap/evict loops, role-claim conflict resolution, and the
delivery/event delegating shims. The worst single spot is **`allocate_and_register`,
a ~371-line method** (`peer_registry.py:909-1280`) braiding display-name
collision, pane-hijack guards, sticky-orchestrator logic, mapping adoption, and
peer construction.

This finding **reinforces `registry-route-split-plan.md` PR4** (extract
`repair.py`) and adds two slices that plan leaves implicit:

1. **`SessionMappingStore`** — mirror `daemon/state/session_bindings.py` to give mapping
   persistence a transactional home and remove inline SQL. (The
   sqlite-state-expansion plan deliberately keeps mappings separate from session
   bindings; this respects that — it just gives mappings a real store class
   rather than raw SQL in the registry.)
2. **`PeerAllocator`** for the 371-line method.

One correctness note for PR4: the registry now has two event-ish surfaces:
durable dashboard/timeline events through `EventLog`, and ephemeral in-process
peer-status pub/sub through `EventBus` (`peer_registry.py:184-185`). They are not
currently duplicate writes of the same event, but they can drift semantically as
more lifecycle events become first-class. Since the roadmap wants approval/memory
changes to be **first-class timeline events**, the repair/event work should make
the durable `EventLog` path the clear source of timeline truth and keep
`EventBus` scoped to transient daemon-internal notifications.

Together these take the file toward ~1800 LOC and make identity/mapping reusable
when sessions become the durable unit.

## Finding 3 — Backend extensibility leaks out of the registry (session-native enabler)

The `AgentBackend` ABC + `AGENT_BACKENDS` registry (`agent_backends.py`) is the
best-factored part of the system: adding a backend is mostly subclass + set
ClassVars. Two responsibilities have escaped it:

- **Resume validation** lives in `session/history.py` as a `_RESUME_VALIDATORS`
  dict plus per-backend `_claude_resumable` / `_codex_resumable` / … functions
  (`session/history.py:190-329`).
- **History loading / discovery** is a set of `if backend == …` chains in the same
  file (`session/history.py:165-450`); ~15 scattered backend-type checks repo-wide
  concentrate here.

Adding a backend therefore means editing `session/history.py`, not just
subclassing. **Direction:** push `is_session_resumable()` and `load_history()`
down as `AgentBackend` methods; the dict and the if/elif chains dissolve into
normal subclass overrides.

This is also the cleanest **session-native unblocker**: it keeps resume *proof*
(does the session exist on disk?), resume *decision* (should we?), and resume
*command* (how?) behind the same backend boundary. Current user-facing restart,
session-control, and job-runner paths already pre-validate via `resume_target()`
or `resolve_resume_safety()` before attaching a resume plan; the remaining seam
is that `SpawnService` trusts callers to pass an already validated
`AgentResumePlan`, while the backend-specific proof still lives in
`session/history.py` instead of the backend registry.

## Finding 4 — `Config` is a god-object (broad import surface)

CLAUDE.md already flags `Config` as the densest, highest-blast-radius node; this
scan confirms the broad import surface: current main has 14 direct importers of
the `Config` class, 33 Python files mentioning `Config`, and many more modules
depending on `config.models` for adjacent exports. Spawn code, hooks, installers,
and the relay client often import a broad config module to read one slice.

- **Free win, zero risk:** `AgentType` is re-exported through `config.models` but
  already lives in `agent_types.py`. Several files import `AgentType` from
  `config.models` even though they do not need the broader config model; repoint
  those imports and they stop depending on `config.models` for an enum-only use.
- **Structural:** slice `SpawnSettings` into its own module so
  `spawn.py` / `spawn_service.py` / `routes/spawn.py` / tests depend on a narrow
  config; do the same for `RelayConfig`. This shrinks the config blast radius and
  makes spawn/relay tests constructable without a full `Config`.

## Finding 5 — `cli.py` is a 4406-LOC monolith

Click-based, ~30 command groups in one file, helpers buried at module level,
`httpx.Client()` reconstructed inline throughout. Not blocking, but it's the file
no one wants to touch. Once Finding 1's services + the `client.py` sync wrapper
exist, splitting into `cli/commands/{peer,jobs,spawn,…}.py` is mechanical. Do it
**after** the service extraction, not before — otherwise you just relocate the
duplication.

## Lower-priority seams (worth noting, not urgent)

- **TS installer duplication.** `installers/opencode.py` (1116) and
  `installers/pi.py` (998) share ~45% near-identical WebSocket / reconnect /
  peer-id-cache glue. Extract a shared TS `WebSocketPeerClient`. Pure
  implementation duplication, not an abstraction gap.
- **Hooks are tmux-welded.** Runtime differences are cleanly normalized in
  `hooks/adapters.py`, but every handler imports `hooks/_tmux` directly. A
  `HookTransport` protocol would be the seam if SSH/container injection is ever on
  the table. No action needed until then.
- **Transport choice is decided once per ask** (`transport_router.py`) and assumes
  a peer's transport is stable for life. Fine for today's ACP subprocesses; revisit
  when a session can swap executors underneath (the roadmap's "peers as live
  runtime executors").

## Prioritized roadmap

| # | Refactor | Unlocks | Effort | Risk | Relation to existing plans |
|---|----------|---------|--------|------|----------------------------|
| 1 | Application-service layer (ask/ack first); route MCP + CLI through it | Shared command surface; kills CLI/MCP/HTTP triplication | High, incremental | Low — DI seam exists | Extends registry-route-split-plan outward |
| 2 | CLI → `client.py` via sync wrapper; drop inline `httpx` | One transport path; unifies `/query` vs `/ask` | Low | Low | New |
| 3 | `is_session_resumable()` + `load_history()` into `AgentBackend` | Session-native resume; clean backend adds | Medium | Low | New |
| 4 | Config slices: move `AgentType` import; carve `SpawnSettings`/`RelayConfig` | Shrinks blast radius; testable subsystems | Low→Med | Low | New (CLAUDE.md notes, no RFC) |
| 5 | Registry split: `SessionMappingStore`, `PeerAllocator`, unified `emit()` | Trustworthy timeline events; identity reuse | Med→High | Medium | Reinforces/extends registry-route-split-plan PR4 |
| 6 | Promote `SpawnService` to own validation/command-resolution; dedupe routes | Robust restart/switch-backend paths | Medium | Medium | New |
| 7 | Shared TS `WebSocketPeerClient`; `HookTransport` protocol | New transports/installers | Medium | Low | New |

The through-line: items 1, 3, and 5 are the same move applied to three hubs —
*pull logic out of the layer that grew it, into a named seam the rest of the mesh
can reuse.* That is also the cheapest path to the session-native architecture the
docs already commit to. Begin with item 1 (ask/ack); everything downstream gets
easier once the service pattern is proven end-to-end.

## Method and caveats

Findings come from a top-down read plus four parallel subsystem deep-dives
(daemon core; transport/backend; surface layers; hooks/spawn/config),
cross-checked against `docs/concepts/session-native-roadmap.md`, the existing
RFCs, and direct file/line verification of the load-bearing claims. Line numbers
are approximate and will drift; treat them as pointers, not anchors. No code was
changed in this pass.
