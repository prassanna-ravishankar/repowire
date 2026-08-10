# CLAUDE.md

Repowire is a mesh network for AI coding agents: Claude Code, Codex, Gemini, OpenCode, antigravity, and pi sessions get a mesh address and talk to each other (ask/ack, notify, broadcast), steerable from a browser, phone (Telegram/Slack), or other agents.

This file is orientation, not a rulebook. It captures how the project thinks and the things that bite you in a long session. For the detailed per-feature reference, the `docs/` tree is the source of truth (and is mirrored into the graphify knowledge graph in `graphify-out/`).

## Philosophies

These are the load-bearing ideas. When a change feels off, it's usually because it fights one of these.

- **Lazy repair, never poll.** Nothing runs on a timer. Liveness reconciliation, persistence flushes, and ghost eviction are deferred until a real request needs them, then piggy-backed on it (`lazy_repair()` runs at most ~1x/30s, triggered by endpoints; disk writes are debounced via dirty flags). If you're reaching for a polling loop or a periodic timer, look for the request you can hang the work off instead.
- **Fail loud, don't paper over.** Delivery that can't happen should surface, not silently degrade. Recent work made `/ask` fail loudly when tmux injection fails, added delivery-trace truthfulness (only claim `pane_injected` when the hook actually acked), and emits `peer_contradiction` events when a peer is in a self-inconsistent state. Prefer a visible warning + safe fallback over a quiet wrong result.
- **The daemon is the only hub; transports are client-side.** Every peer speaks the same WebSocket protocol to `daemon-go/hub`. How a peer connects (hooks, channel/ACP, relay, bot) is a client concern. Routing, identity, and lifecycle live in the daemon and shouldn't learn about specific transports.
- **Session-native is the direction, not the present.** Sessions are becoming the durable unit of work; peers stay live runtime executors. Ask/notify already route through a transport router; resume is captured per backend. Frame model-switching, plan approval, universal transport-neutral control, and production ACP as roadmap — don't claim them done.
- **Identity is `peer_id`, addressing is `display_name`.** Display names collide (spawned same-path peers); the daemon canonicalizes routing-sensitive state to `peer_id`. When something misroutes or 500s intermittently, suspect a display-name lookup that should have been a peer_id.
- **Leave it better-factored; prefer the simplest description.** Read before writing, extract before duplicating. Among comparable designs, the one with the shortest honest description usually wins (Schmidhuber). The recent resume work consolidated three near-duplicate `_resume_plan_for` implementations into one `resume_safety` seam — that's the move.
- **Pre-validate destructive actions.** Resume-capable backends exit hard on a bad session id, so restart pre-validates the session file exists *before* killing the pane. Kill/restart require destructive pane proof: durable spawn ownership, or live pane metadata whose `peer_id` matches the target. Path/cwd alone is not enough. The pattern: prove the irreversible step is safe before taking it.

## Long-running session notes

Things that have cost time before. Not exhaustive, and some may drift — verify against the live system rather than trusting this blindly.

- **Hooks run from the binary recorded by setup.** After changing `daemon-go/hooks/` (or daemon code you want live), rebuild that binary, then re-run `repowire setup` when installer output changed. Editing the source tree alone does nothing for a running agent.
- **Restarting the daemon bounces the whole mesh.** `repowire service restart` re-registers every peer (including the session you're in and any spawned reviewers). Expected, but it interrupts in-flight work — sequence it deliberately. For a live test, spawn a *throwaway* peer rather than restarting yourself.
- **Verify UI/behavior changes live, not just by green gates.** A passing build or a 200 response is not proof. A landing-page fix shipped "green" twice while still rendering blank in the browser; a `resume_mode=resumed` field meant nothing until a resumed peer actually recalled prior context. Use agent-browser for web, a real round-trip for delivery/resume.
- **`gh pr edit` is broken on the installed `gh` (2.54.0)** — it queries deprecated Projects-classic GraphQL and 400s on any edit. Set the PR body/labels via REST instead: `gh api -X PATCH repos/OWNER/REPO/pulls/N -F body=@file`. `gh pr comment` works fine. (gh-pr-flow's "use gh api directly" is the general lesson.)
- **`.beads/issues.jsonl` churns on every `bd` command** (auto-export). It's local noise — `git checkout .beads/issues.jsonl` before committing product changes; the pre-PR hygiene check fails on ledger churn by design.
- **bd runs on the dolt backend; don't trust its error hints.** If `bd create` suggests `bd init` or `--no-db`, do neither — `bd list`/`create`/`close` work directly against the existing dolt db. `--no-db` mode is broken here (`issues.jsonl` line 104 is malformed — string comment id — and JSONL-only mode refuses to load; the dolt path just skips it with a warning). A failed bd invocation can leave a blank empty-id issue behind; `bd delete "" --force` doesn't work (no tombstone support), `bd close ""` does.
- **Backend-detection tests must be env-hermetic.** `detect_mcp_backend` reads the process env, so a test that `patch.dict`s only `PATH` still inherits the ambient `CLAUDE_CODE_*`/`AI_AGENT` of the host session and mis-detects `claude-code`. (This bit the old `TestMcpRegistration` "codex vs claude-code" failures, fixed in #330 by nulling the conflicting markers.) Null the other runtimes' markers when asserting a specific backend.
- **Codex registers late** (after its first interaction, not at startup); its hook payload still carries the runtime `session_id` like Claude's. MCP lazy registration covers the gap.
- **graphify finding (refresh periodically):** post-v0.15.2 refactor, the highest-blast-radius surface is the `Peer` type family — `Peer` (degree 340), `PeerRole` (332), `PeerStatus` (299), then `PeerRegistry` (263). `Config` dropped to 8th (175) after the config-import slicing, so it is no longer the densest node. Treat the `Peer`/`PeerRole`/`PeerStatus` data types as the things to keep stable; registry/identity/spawn still form one tightly-coupled community (`c0`, ~984 nodes) despite the file-level `registry_*` split, because they share the `Peer` data shape. (Graph refreshed 2026-06-06 at v0.15.2: 7374 nodes / 277 communities.)

## Build, test, release

```bash
mkdir -p bin && cd daemon-go
gofmt -w . && go vet ./... && go test -race ./...
go build -o ../bin/repowire .           # hooks run from this recorded binary
cd ../web && npm test -- --run && npm run build
```

CI runs Go vet/race tests, web tests/build, release cross-compiles, and the relay image build (`.github/workflows/ci.yml`). Channel server deps: `cd daemon-go/cli/assets/channel && bun install`.

**Release:** update `daemon-go/cli.Version`, commit, tag, push — CI publishes native GitHub Release archives from the tag, and `relay.yml` redeploys the relay/web on relevant changes.
```bash
git tag v0.X.Y && git push origin main --tags
```
Versioning: patch (0.x.Y) for fixes/cleanup/small additions, minor (0.X.0) for significant features or breaking changes. After 0.9.x → 0.10.0, 0.11.0… **never auto-bump to 1.0.0** — that's Prass's call. Merge PRs with `gh pr merge <N> --squash --delete-branch`, then checkout main, pull, release.

**Testing idioms:** route and WebSocket tests use `httptest` plus the native WebSocket client. Keep runtime-detection tests environment-hermetic, and avoid leaking ws-hooks to the live daemon.

## Architecture

```
                    ┌─────────────────────────────────┐
                    │ Go daemon (daemon-go/hub), :8377│
                    │ Registry · Router · AskTracker  │
                    │ SQLite · HTTP MCP `/mcp`        │
                    └──────────┬───────────────────┘
                               │ WebSocket /ws
            ┌──────────────────┼──────────────────┐
   Hooks transport      Channel/ACP transport   Other peers
   (default)            (experimental)          (relay, Telegram, Slack, OpenCode)
 daemon-go/hooks/ws.go  channel/server.ts
   (tmux injection)     (MCP stdio)
```

Key modules (the graph in `graphify-out/` is the live map; these are the hubs):

- `daemon-go/peer/` — peer state, circle access, events, lazy repair, ghost eviction, contradiction events
- `daemon-go/service/` — message/transport routing (ACP-before-WS), delivery, asks, spawn, resume safety, sessions, schedules, and jobs
- `daemon-go/hub/` — HTTP/WebSocket routes plus all 31 MCP tools at `/mcp`
- `daemon-go/state/` — schema-versioned SQLite state and one-time legacy JSON imports
- `daemon-go/hooks/` — native session/stop/prompt/notification/pre-tool hooks, ws-hook, transcript parsing, and chat streaming
- `daemon-go/cli/` — native CLI, setup/installers, and embedded runtime/channel/orchestrator assets
- `daemon-go/mcpstdio/` — thin per-session identity proxy from MCP stdio to daemon HTTP MCP
- `daemon-go/service/agent_backends.go` — per-backend command, capability, and resume configuration
- `daemon-go/relayserver/` + `daemon-go/relay/client.go` — hosted relay server plus daemon relay client
- `daemon-go/mobile/` — native Telegram and Slack bot peers (Socket Mode / inline buttons), sticky routing; `@telegram`/`@dashboard` are the human

### Transport mechanics worth knowing

- **Hooks (default):** SessionStart registers the peer + spawns the ws-hook (flock-deduped) + injects context. Stop posts chat turns, drains any old legacy `/query` FIFO response, then fetches `/asks/pending` and emits `{"decision":"block","reason":<reminder>}` if open asks exist (Stop can't add context otherwise), then marks online. UserPromptSubmit → BUSY; Notification(idle_prompt) → ONLINE. The ws-hook injects `[ask #cid]` text on arrival; the Stop reminder is the *only* thing that resurfaces an un-acked ask.
- **Channel (experimental, `repowire setup --experimental-channels`):** Claude Code ↔ `channel/server.ts` (MCP stdio) ↔ daemon. Messages arrive as `<channel>` tags; Claude replies via the `reply`/`ack` tools. Requires claude.ai login, Claude Code 2.1.80+, bun. Only the Stop hook is kept in channel mode (for dashboard chat turns).
- **Config** lives at `~/.repowire/config.yaml` (`daemon`, `relay`, `telegram`, `slack`, `updates`, `experiments`, …) and is loaded through `daemon-go/config`. Channel/MCP config is in `~/.claude.json`, managed by `repowire setup`.

## Docs, memory, graph

- **Public docs (`docs/`) are part of the change.** Behavior changes update the relevant docs in the same PR — README for install/quickstart/major features, `docs/reference/*` for CLI/MCP/HTTP/config surfaces, `docs/guides|capabilities|concepts|patterns|operations/*` for the rest. If you intentionally defer, file a Beads follow-up and say so in the handoff. `scripts/pre-pr-hygiene.sh` is an advisory pre-PR check (points at docs surfaces, fails on beads-ledger churn). Screenshots: browser-generated only, never AI-mockups.
- **Knowledge graph:** `graphify-out/` holds a graphify graph; refresh after significant changes with `/graphify . --update`. Useful for "what touches X / how does A reach B" questions grep can't answer. Don't paste generated JSON into hand-written docs.
- **Memory** is project-local at `.claude/memory/` (committed, public) — and `bd remember` for beads-tracked knowledge. Public-repo sanitization: no secrets, no absolute/home paths (repo-relative only), no IPs/hostnames/personal identifiers. When in doubt, omit.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
