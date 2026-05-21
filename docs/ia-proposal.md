# Docs site IA proposal (issue #126)

Status: **DRAFT — awaiting orchestrator sign-off before content writing.**
Branch: `feat/docs-site`. Target: `docs.repowire.io`.

## Phase 1: audit findings

### Existing surface

| Source | What it covers | State |
|---|---|---|
| `README.md` | Why, install, quickstart, how-it-works, supported agents, patterns (7 collapsed), dashboard, Telegram bot, MCP tools table, CLI ref, config, security, uninstall | Comprehensive single-page; PR #144 refresh is in flight — do not touch |
| `web/app/page.tsx` (landing) | Hero, Features (6), HowItWorks (4 steps), Installation (uv copy-block), Footer | Marketing-y but on-brand; references `#installation`, GitHub, dashboard only |
| `web/app/docs/page.tsx` + `_nav.ts` | Docs index with sidebar nav | Stub, links to 5 pages below |
| `web/app/docs/quickstart/page.tsx` | 3-step install → setup → ask | Well written, current with v0.13 |
| `web/app/docs/concepts/page.tsx` | Peers, circles, message types, lazy repair, orchestrator, control surfaces | Well written, current |
| `web/app/docs/reference/tools/page.tsx` | All 9 MCP tools with signatures + examples | Well written, current |
| `web/app/docs/reference/cli/page.tsx` | 7 `repowire` subcommands | Slim but accurate |
| `web/app/docs/reference/client/page.tsx` | `AsyncRepowireClient` (construction, identity, routing, lifecycle, errors, stability) | Well written, current |
| `docs/design-system.md` | Copper Mesh design system (voice, color, type, components) | Internal reference; not user docs |

### What is already explained well

1. Mental model of `ask`/`ack`/`notify`/`broadcast` (README + `concepts`).
2. The minimum-friction first run (3-step quickstart).
3. The MCP tool surface (`reference/tools` is reference-grade).
4. The Python client surface (`reference/client`).
5. Why a daemon, not pub/sub (README "How It Works" + `concepts` lazy-repair section).

### Gaps (no current docs)

- **Per-agent install specifics.** Codex `config.toml` quirks, Gemini `BeforeAgent`/`AfterAgent` mapping, OpenCode TypeScript plugin, channel-mode opt-in. Auto-detected by `repowire setup` but undocumented when it goes sideways.
- **Telegram bot.** README has a paragraph, no dedicated page. Sticky routing, inline buttons, `@telegram` framing, env-vs-config setup.
- **Slack bot.** One paragraph in README. Socket Mode setup, Block Kit pickers, channel scoping.
- **Dashboard.** README has screenshots and one paragraph. Compose bar, attachment upload, peer chat, live mesh log, remote dashboard via relay.
- **Relay.** README has a collapsed section. Outbound WSS topology, cookie auth, self-hosting, HTTP tunnel mechanics, security posture.
- **Orchestrator pattern.** Mentioned in README as a one-line collapsed pattern; mentioned in `concepts` as a workflow. No how-to.
- **Troubleshooting.** Zero coverage. Hooks not firing, daemon not reachable, ws-hook dedup, ghost peers, relay key rotation, channel-mode auth, MCP server identity drift on Codex.
- **Comparisons.** README has the small Gastown / Claude Squad / Memory Bank table. No standalone pages.
- **Configuration deep-dive.** README dumps a YAML example. No per-key reference (spawn runtime profile semantics, prune_max_age_hours, auth_token effect on local-only vs relay).
- **Architecture deep-dive.** README has an image and a paragraph. Daemon module map (PeerRegistry, MessageRouter, AskTracker, QueryTracker, WebSocketTransport), transport layer (hooks vs channel), lazy-repair tradeoffs.

### Critical IA decision the brief should choose between

The brief specifies **mkdocs-material**. There is already a **Next.js docs scaffold** at `web/app/docs/*` with 5 written pages matching the Copper Mesh design system and shipping in the same Docker image as the dashboard.

Two paths:

**A. mkdocs-material at `docs.repowire.io` (per brief).**
- Pro: standard tooling, fast search, plugins (mermaid, social cards), low-effort theming, very common for OSS infra.
- Pro: separate from product surface — docs ship out-of-cycle from product builds.
- Con: design system divergence. Copper Mesh is a custom tailwind-v4 token system; matching it in mkdocs needs a custom theme.
- Con: requires deciding the fate of `web/app/docs/*`. Either delete (lose work) or keep as a stub redirecting to `docs.repowire.io`.
- Con: clusterkit must build a second container (mkdocs static site) and HTTPRoute.

**B. Keep Next.js docs route under `docs.repowire.io` (same code, new host).**
- Pro: zero design drift, already partially written, same i18n future, same fonts.
- Pro: clusterkit can reuse the existing `web` container behind a new HTTPRoute that resolves `docs.repowire.io` to `/docs/*`.
- Con: docs and dashboard ship together (rebuild on either changing).
- Con: no built-in search, no mkdocs plugin ecosystem, all components hand-rolled.

**Recommendation: A (mkdocs-material), with deletion of `web/app/docs/*` after content port.** Reasons:
1. The brief is explicit, and docs.repowire.io is a public-docs subdomain, not a product subroute.
2. The existing Next.js pages are well-written *prose*, which ports to markdown cleanly in <1 day per page. The design loss is mitigated by mkdocs-material's `custom_dir` overrides (we can pull Copper tokens into a thin material theme override).
3. Decoupling docs builds from product builds reduces dashboard churn for content edits.

If sign-off pushes back, **B** is a viable fallback that ships faster.

## Phase 2: proposed IA (mkdocs-material)

Top-level structure (mkdocs nav):

```
Home (index.md)
  └── one-page "what is repowire / install / first ask" entry

Quickstart
  ├── Install
  ├── Setup
  └── First ask (cross-repo)

Concepts
  ├── Peers and circles
  ├── Message types (ask, ack, notify, broadcast)
  ├── Lazy repair (no polling)
  ├── Control surfaces (dashboard, Telegram, Slack)
  └── Orchestrator pattern

Setup per agent
  ├── Claude Code (hooks + MCP; channel mode opt-in)
  ├── Codex (hooks + MCP; SessionStart timing)
  ├── Gemini CLI (BeforeAgent / AfterAgent mapping)
  └── OpenCode (TypeScript plugin)

Control surfaces
  ├── Web dashboard
  ├── Telegram bot
  └── Slack bot

Relay (remote access)
  ├── Hosted relay (repowire.io)
  ├── Self-hosting the relay
  └── Security posture

Patterns (cookbook)
  ├── Multi-repo coordination
  ├── Cross-agent review
  ├── Orchestrator coordination
  ├── Worktree isolation
  ├── Mobile mesh management
  ├── Infrastructure-as-peer
  └── Overnight autonomy

Reference
  ├── MCP tools
  ├── Python client (AsyncRepowireClient)
  ├── CLI (repowire …)
  ├── Configuration (~/.repowire/config.yaml)
  └── Architecture (daemon modules, transports, wire protocol)

Troubleshooting
  ├── Hooks not firing
  ├── Daemon unreachable
  ├── Ghost peers / stuck busy state
  ├── Channel-mode auth failures
  ├── Relay key rotation
  └── Diagnostic commands (repowire status, doctor)

Comparisons
  ├── vs Happy
  ├── vs cloud coding services (Devin, Cursor BG, etc.)
  ├── vs Memory Bank
  ├── vs Claude Squad
  └── vs Gastown

About
  ├── Versioning
  ├── License (MIT)
  └── Contributing
```

### What each top-level section is for

- **Home**: 60-second "what / install / minimum example" — the one URL someone shares on a thread.
- **Quickstart**: assumes nothing; gets a working two-peer ask in five minutes.
- **Concepts**: the mental model needed to read the rest. Read once, refer rarely.
- **Setup per agent**: only opened when something breaks or when adding a new runtime.
- **Control surfaces**: how humans drive the mesh.
- **Relay**: only relevant when going remote / multi-machine.
- **Patterns**: cookbook for non-trivial workflows. Outcome-oriented, not feature-oriented.
- **Reference**: deep-end stuff. Stable signatures and exhaustive lists.
- **Troubleshooting**: symptom → cause → fix. Heavy `grep`-friendly headings.
- **Comparisons**: workflow boundary, never feature-parity. Each page answers "when would you reach for X instead of repowire, and vice versa?"
- **About**: small.

### Comparison page treatment (per brief constraint)

Each comparison page follows the same shape:

1. **What it is** (one paragraph, from their docs).
2. **Workflow boundary** (when do you reach for it? what does it not try to do?).
3. **Architecture difference** (sync vs async, persistent vs ephemeral, single-machine vs cloud, etc.).
4. **When to use repowire instead.**
5. **When to use the other tool instead.**
6. **Can you use both?** (often yes).

No feature-parity tables. No "we win on X". No financial framing.

Targets:
- **Happy** — fork of Claude Code with mobile/web access, persistent sessions.
- **Cloud coding services** (Devin, Cursor Background Agents, Codex Cloud, etc.) — remote-machine autonomous agents.
- **Memory Bank** — async, file-based persistent context.
- **Claude Squad** — local session manager (tmux + worktrees).
- **Gastown** — async work orchestration with persistent mail (already in README's small table; expand here).

## Phase 3: tech proposal

- **Generator:** `mkdocs-material` (Material for MkDocs).
- **Host path:** `docs/` at the repo root (existing `docs/design-system.md` is unchanged — stays as internal reference).
  - `docs/index.md`, `docs/quickstart/*.md`, etc. (slugs match the IA above).
  - `mkdocs.yml` at repo root.
- **Plugins:** `search`, `mermaid2` (for architecture diagrams), `social` (OG cards), `awesome-pages` (folder nav), `glightbox` (zoomable diagrams).
- **Theming:**
  - Use the Material "slate" palette as the base, with a thin custom `overrides/` folder that injects the Copper Mesh tokens (copper-500 `#C77B3D`, ink-950 `#0F0E0C`, signal-300 `#5BA3F5`) via CSS variables.
  - Self-host JetBrains Mono + IBM Plex Sans from `docs/stylesheets/`.
  - No emoji in chrome (per design system).
- **Build:** `uv run mkdocs build` produces `site/`. Local preview `uv run mkdocs serve`.
- **Dependency surface:** add `mkdocs-material`, `mkdocs-mermaid2-plugin`, `mkdocs-awesome-pages-plugin`, `mkdocs-glightbox` to a new optional `[project.optional-dependencies] docs` group in `pyproject.toml`. Does not change runtime dependencies.
- **Deployment:** clusterkit builds a small static-serving container (`nginx:alpine` over `site/`) for the `repowire-docs` Helm chart. HTTPRoute for `docs.repowire.io`. Coordinated separately after IA sign-off.
- **CI:** add a `docs` job to `.github/workflows/ci.yml` that runs `mkdocs build --strict` so broken links and missing nav entries fail PRs. Separate build/push workflow for the docs container (mirrors the relay workflow shape).

### What does **not** change (hard constraints honored)

- `web/` Next.js dashboard and its relay endpoints: untouched.
- Daemon code: untouched.
- `README.md`: untouched (PR #144 owns it).
- Existing `docs/design-system.md`: untouched, kept as internal reference.

### What this proposal proposes to remove (subject to sign-off)

- `web/app/docs/*` directory after content is ported to mkdocs and `docs.repowire.io` is live, **only if** path A is chosen. Until then, kept as-is.

## Open questions for sign-off

1. **Path A (mkdocs) or path B (keep Next.js docs route)?**
2. **Versioning of docs:** static (latest only) or `mike`-versioned (per-tag)? Recommendation: latest only until first stable release.
3. **Search:** built-in lunr (offline, default) or Algolia DocSearch (hosted)? Recommendation: built-in for now; revisit when traffic justifies Algolia.
4. **Comparison page tone:** the brief says "workflow boundary, never misleading feature parity." Confirm the 6-section template above matches your intent.
5. **Where does the existing landing page (`web/app/page.tsx`) point?** Today `Get started` jumps to `#installation` on the same page. Should this change to `docs.repowire.io/quickstart` after launch?

## Next steps once signed off

1. Scaffold `mkdocs.yml` + `docs/index.md` skeleton + nav stubs (one empty `.md` per IA leaf).
2. Wire `pyproject.toml` docs extras + `mkdocs build --strict` CI job.
3. Port the 5 existing Next.js doc pages to markdown (low-risk, prose-only edits).
4. Write the gap pages from highest to lowest leverage:
   - Troubleshooting (highest ROI, single biggest gap).
   - Per-agent install pages.
   - Control surfaces (dashboard, Telegram, Slack).
   - Relay.
   - Patterns cookbook.
   - Comparison pages.
5. Coordinate with `clusterkit-claude-code` for the Helm chart + HTTPRoute for `docs.repowire.io`.
6. Open PR, do not merge.
