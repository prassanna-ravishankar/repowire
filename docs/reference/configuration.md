# Configuration

Config lives at `~/.repowire/config.yaml`. `repowire setup` creates the file with `0600` permissions.

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "rw_local_..."
  circle_boundary: session
  delivery_queue_ttl_seconds: 86400
  delivery_queue_max_per_peer: 100
  orchestrator_recall:
    enabled: true
    max_hits: 3
    max_chars: 900
    max_file_chars: 12000
  mcp_http:
    enabled: true
    bind: "localhost-only"
    require_auth: true
    allow_unauthenticated_localhost: false
    allow_dangerous_tools: false
  spawn:
    commands:
      claude-code: "claude --dangerously-skip-permissions"
      codex: "codex --dangerously-bypass-approvals-and-sandbox"
      pi: "pi"
      opencode: "opencode"
      pi: "pi"
    profiles:
      codex:
        fast:
          args: ["--model", "gpt-5-mini"]
          description: "Lower-latency Codex peer"
        capable:
          args: ["--model", "gpt-5"]
          description: "More capable Codex peer"
    allowed_paths: []
updates:
  check_enabled: false
telegram:
  bot_token: ""
  chat_id: ""
slack:
  bot_token: ""
  app_token: ""
  channel_id: ""
```

## Environment variables

Documented scalar daemon, relay, and experiment fields can be overridden
by environment variables. Nested fields use a `REPOWIRE_` prefix and `__` as the
section delimiter:

```bash
REPOWIRE_DAEMON__PORT=9000
REPOWIRE_DAEMON__AUTH_TOKEN=rw_...
REPOWIRE_DAEMON__CIRCLE_BOUNDARY=window
REPOWIRE_RELAY__URL=wss://repowire.io
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_CHANNEL_ID=C...
```

Two legacy flat aliases are kept for the relay: `REPOWIRE_RELAY_URL` and `REPOWIRE_API_KEY` (setting `REPOWIRE_API_KEY` also flips `relay.enabled` to true).

Telegram and Slack accept both their conventional flat environment variables
shown above and nested `REPOWIRE_TELEGRAM__*` / `REPOWIRE_SLACK__*` aliases.
When all required credentials for a transport are present during `repowire setup`,
Repowire installs that bot as an OS-managed user service alongside the daemon.
Removing or clearing those credentials and running setup again removes the
corresponding managed service. The transport remains a client of the daemon; it
does not run inside the daemon or in tmux.

Resolution precedence, highest first: explicit constructor arguments, the flat relay aliases, `REPOWIRE_*` environment variables, `~/.repowire/config.yaml`, then built-in defaults. Environment variables take precedence over the config file, so an exported `REPOWIRE_DAEMON__PORT` overrides `daemon.port` in the YAML.

## `daemon.auth_token`

Optional local bearer token for daemon HTTP routes, WebSocket connections,
hooks, and HTTP MCP. `repowire setup` generates one automatically because the
stdio MCP identity shim forwards to `/mcp`. Treat it as a local password;
rotate it by replacing the value and restarting the daemon service.
The daemon-served localhost dashboard may call same-origin HTTP routes without
the token; other local HTTP clients still require the bearer header.

## `daemon.circle_boundary`

Controls which tmux container supplies the implicit circle. `session` is the
default and preserves existing behavior: every peer in a tmux session shares
that session's circle. `window` gives each tmux window its own stable circle,
named from tmux's window id (for example `window-7`); panes in that window share
the circle even if the window is renamed. The only accepted values are
`session` and `window`; invalid configuration stops loading instead of silently
falling back.

Changing this setting does not rewrite durable peer identities already recorded
in the daemon. Stop and recreate peers that should adopt the new boundary.
Restart and backend-switch refuse to replace the last pane in a window, because
killing it would destroy the window before tmux could place its replacement.

## `daemon.mcp_http`

Streamable HTTP MCP endpoint mounted at `http://127.0.0.1:8377/mcp`. The daemon
owns all tool implementations here; `repowire mcp` is a per-agent stdio proxy.

- `enabled`: mount `/mcp`. The model default is `false`; `repowire setup` enables it for normal installations.
- `bind`: only `localhost-only` is supported. The handler rejects non-loopback callers even when the daemon itself listens on a wider interface.
- `require_auth`: require `Authorization: Bearer <daemon.auth_token>`. Default: `true`.
- `allow_unauthenticated_localhost`: development-only escape hatch that disables bearer auth when `require_auth` is also disabled. Do not use on shared machines.
- `allow_dangerous_tools`: allow lifecycle/admin MCP tools over HTTP MCP. Default: `false`; spawn, kill, and schedule mutation stay disabled.

HTTP MCP is never exposed through the hosted relay. The stdio identity shim is
the stable path for agents because it preserves peer identity; direct HTTP is
for local steering clients that accept the `mcp-http` identity and restricted
admin surface.

## `daemon.delivery_queue_*`

Repowire keeps a small SQLite-backed delivery queue for peers that miss a live WebSocket delivery and later poll from a Stop hook or CLI fallback. Live delivery is always attempted first. If the live transport is unavailable, notifications are queued; asks to CLI-fallback peers are queued for one-shot delivery while the open ask thread remains visible through `/asks/pending` until `ack`.

- `delivery_queue_ttl_seconds`: how long a queued delivery remains drainable. Default: `86400` (24 h). Set `0` to disable queued delivery.
- `delivery_queue_max_per_peer`: maximum queued rows retained per peer. Default: `100`. Oldest rows are evicted when the cap is exceeded. Set `0` to disable queued delivery.

Queued notifications may also replay on a successful WebSocket reconnect for the same peer. Other draining is delete-on-read through the Stop hook or `repowire peer deliveries`, so the same queued paste is not replayed indefinitely. Ask reminders are separate: open asks continue to appear through `/asks/pending` until closed.

## `daemon.orchestrator_recall`

Daemon-side inbound recall triage for peers registered with `role=orchestrator`.
Before an ask or notification is handed to the transport, the daemon does a
bounded lexical scan of the orchestrator workspace context (`comms.md`,
`projects.md`, and `memory/*.md`) using the inbound text plus sender/target
metadata. When there is a match, the delivered message is prefixed with a small
`[repowire recall]` block containing the top hits. The hook is keyed only off
the peer's registered role; it does not depend on persona text or any runtime
self-attestation.

- `enabled`: turn recall injection on/off. Default: `true`.
- `max_hits`: maximum matched files to include. Default: `3`.
- `max_chars`: maximum injected block size. Default: `900`.
- `max_file_chars`: maximum characters read from any one source file. Default: `12000`.

## `daemon.spawn`

Spawn is disabled until `allowed_paths` and at least one runtime command are configured. `commands` is keyed by backend (`claude-code`, `codex`, `opencode`, `pi`) and is the single launch profile used by MCP `spawn_peer`, dashboard spawn and backend forking, `repowire peer restart`, and `repowire orchestrator start`. Dashboard backend forking is non-destructive: it starts an agent-role sibling in the source peer's project and circle while the source keeps running. It works for both Repowire-managed tmux peers and pane-less native sessions. Backend-native conversation history is not copied. Legacy Gemini or Antigravity keys are ignored.

Backend commands are reloaded from config when a spawn request needs them, so
`repowire setup` and manual command edits take effect without restarting the
daemon or bouncing existing agent sessions.

`profiles` is optional and keyed first by backend, then by a user-defined profile name. Each profile appends structured `args` to the configured backend command; Repowire does not hardcode provider model names. For example, spawning `codex` with profile `fast` runs the configured `daemon.spawn.commands.codex` command plus the profile args. Profile descriptions are informational and may be shown by UIs.

`env_path` and `env` define the environment injected into spawned agent commands.
`env_path` becomes the spawned process `PATH`; `env` adds extra variables such as
tool config paths. If neither `env.PATH` nor `env_path` is set, Repowire captures
the user's login-shell PATH and injects it so launchd/tmux-spawned workers can
still see tools installed by Homebrew, nvm, and similar shell setup. This env is
used by MCP/dashboard spawn, backend forking, restart, and durable job workers.

Explicit command overrides still win over profiles. `repowire peer restart` preserves the peer's backend, path, circle, role, and mesh identity, but this slice does not persist the selected profile in peer state. Restart therefore uses the current configured backend command unless a future lane records spawn profile metadata.

`allowed_commands` is a deprecated compatibility field. When present in an older config, Repowire normalizes it into `commands` while loading config; new configs should not add it.

## `updates.check_enabled`

Opt-in release availability checks for `repowire status` and `repowire doctor`. Default: `false`.

When enabled, those commands may query GitHub Releases and report that a newer Repowire release is available. They never install binaries, rewrite hooks, restart services, or mutate daemon routing. `repowire update` remains the explicit upgrade path.

## `experiments`

Off-by-default feature flags for code paths not yet ready to be default-on:

```yaml
experiments:
  acp_broker_client: true       # route asks to ACP peers through a broker-side ACP client
  chat_turn_streaming: true     # stream block-level chat_turn_delta events (Claude Code)
  remote_tool_approval:
    enabled: true               # PreToolUse hooks-path remote tool approval (Claude Code)
    gated_tools: [Bash, Edit, Write, MultiEdit, NotebookEdit]
    timeout_seconds: 45
  git_trailers: true            # enables the Git hook installed with setup --git-hooks
```

`remote_tool_approval` gates `gated_tools` behind a blocking approval question: before a gated tool runs, a `PreToolUse` hook posts the question to the daemon and waits for an allow/deny from a human surface or peer, denying on timeout. The installer only registers the `PreToolUse` hook when `enabled` is set; toggling it off and re-running `repowire setup` removes the hook. Read-only tools are never gated. See [Structured questions](../concepts/message-types.md#pretooluse-tool-approval-claude-code).

`git_trailers` is on by default, but requires `repowire setup --git-hooks` in each repository (or manual integration with an existing `prepare-commit-msg` hook). The native Git hook adds thread/session trailers using the committing repository’s HEAD and the registered tmux pane’s peer, across supported agent backends. It never rewrites shell input. Existing attribution, reused messages and replay operations are preserved; empty messages stay empty. Without a registered pane or available daemon history it leaves the message unchanged; operational errors warn and never block Git. Set `false` to disable it immediately, without reinstalling. See [`repowire why`](cli.md#repowire-why) for installation and amend/rebase behavior.

## `routing`

How an ask finds its recipient. Repowire is a router whose default strategy is static: every ask goes to the peer it names. Other strategies are opt-in.

```yaml
routing:
  strategy: static              # static, or the arm that picks a recipient for asks that name none
  record: false                 # write a route_decision trace row for every ask
  record_text: true             # keep the ask body in route_decision rows (off keeps only its SHA-256 there)
  shadow_arms: []               # arms run beside every named ask, for the record (needs record: true)
  sample_every: {}              # e.g. {frontier-claimed: 20} runs that arm on every 20th ask
  timeout_seconds: 8
  allow_service_targets: false  # let a strategy deliver to service- or human-role peers
  jev: {api_key: "", model: jev-1.13.0, base_url: "https://api.typesafe.ai"}
  frontier: {api_key: "", model: claude-opus-5-5}
```

**Named asks are never rerouted.** An ask that names a peer is resolved and authorized as named under every strategy. If the name is unknown, ambiguous, denied or not addressable, the ask fails. It never falls through to a strategy. The strategy is consulted only for an ask that names no peer: `/ask` with the `to_peer` key left out, or the MCP `ask` tool without `peer_name`.

- **`static`** (default) refuses a targetless ask with `400 no_target`. That refusal happens before any trace row or snapshot.
- **An arm name** (`random`, `jev-*`, `frontier-*`) ranks the eligible candidates. Its pick passes the same circle, addressability and self checks as a named target, and the delivered ask is prefixed with the arm and its score. If the arm fails, the ask goes to the sender's `fallback_to` peer, which is authorized the same way, with a note saying so. If the pick stops being eligible before delivery, the reason is `stale_pick:*` and the same fallback applies. Without `fallback_to` the ask is refused with `503 router_unavailable` and nothing is delivered.

A strategy never delivers to a peer whose role is `service` (for example `telegram` or `slack`) or `human`, unless `allow_service_targets: true`. On a targetless decision, those peers are recorded with `access: service_excluded` and are not offered to the arm. Circle and addressability verdicts take precedence. The same rule applies when the pick and `fallback_to` are authorized, so a pick that became a service peer after the snapshot is a `stale_pick:service_excluded` fallback. A `fallback_to` naming one is refused with `403 service_excluded`. The rule does not apply to named asks: name `telegram` and you reach `telegram`. Shadow arms run on named asks, so they still see and score service peers. Each targetless decision row records the policy it ran under in `selection_policy` (`exclude_service_targets` or `allow_service_targets`). Roles are declared by the peers themselves, so this is an operating policy, not a security boundary.

Only an absent target is routed. An empty or whitespace `to_peer`, or JSON `null`, is a named ask to nobody and fails with `404 Unknown peer`, as it always has. `@route`, the experiment's former reserved name, is now an ordinary name. `ask_many` children, the legacy `/query`, scheduled asks, the CLI and the Pi, OpenCode, Telegram, Slack and relay surfaces always name their target.

With `record` on, every ask attempt (`ask`, each `ask_many` child, and the legacy `/query`) writes a `route_decision` row into its delivery trace in `state.db`. The row holds:

- the sender;
- the declared target and how it resolved;
- a snapshot of every registered peer, labelled with whether the sender may reach it, and split into:
  - attested state: backend, project, path, status, turn state, last-seen age, open inbound asks, and last exchange with the sender;
  - claimed state: description and its age.

Outcomes are the trace's later stages, and `closed` gains `time_to_close_ms` and `recipient_turns`. Asks whose target fails to resolve are traced too. The rows are never pruned. `record_text: false` stores only a hash of the ask in `route_decision` rows. Other trace stages are unaffected: `created` keeps a clipped copy of the text and `closed` a clipped reply.

The row's `mode` is derived from the config, so it reads as it did under the experiment:

- `live` when the strategy is not static;
- otherwise `shadow` when shadow arms run;
- otherwise `off`.

Named asks record `arm: explicit` under every strategy. With `record` off and no targetless ask, asks take the same path as without routing: no snapshot and no extra rows. With `record` on, every ask snapshots the mesh, whatever the strategy.

`shadow_arms` run off the request path beside each named ask, and each result is recorded as a `route_shadow` row. Paid arms should carry a `sample_every`. Shadow runs are bounded: at most 4 are in flight, and arms planned while the pool is full are recorded as `saturated` on the decision row itself, so overload adds no write to the ask. Each provider (Jev, Anthropic) sits behind a breaker shared by its shadow and strategy use. The breaker opens after 3 consecutive provider failures, rejects calls as `breaker_open` for 30 seconds, then lets the next request through as a probe. A panicking arm is recorded as a `panic` fallback.

Arms: `random`, and `jev-*` / `frontier-*` over the views `claimed` (descriptions), `attested` (observed state) or `both`. Model arms see candidates under opaque keys, with peer text passed only as data inside a fixed template. `jev.api_key` falls back to `JEV_API_KEY` / `TYPESAFE_API_KEY`, and `frontier.api_key` to `ANTHROPIC_API_KEY`. A model arm without a key falls back with reason `unconfigured`.

A routing setting that cannot take effect is dropped with a startup warning; it does not stop the daemon. For example, `shadow_arms` with `record: false` are dropped, because shadow results exist only in the record. Only typo-class mistakes refuse to load, as they always have: a `shadow_arms` entry listed twice, a `sample_every` below 1, or, at daemon start, a strategy, shadow arm or `sample_every` key that names no arm. `repowire route show` prints the resolved config and any warnings.

### Migrating from `experiments.router`

Routing graduated from the `experiments.router` block. A legacy block is migrated in memory at startup with a warning that prints the equivalent `routing:` block, with API keys redacted. The file is never rewritten, so you can move the keys whenever you like:

| `experiments.router` | `routing` |
|---|---|
| `mode: off` | `strategy: static`. Shadow arms are dropped; they never ran. |
| `mode: shadow` | `strategy: static`, `shadow_arms` kept |
| `mode: live`, `arm: X` | `strategy: X`. Shadow arms are dropped; they never ran in live. |
| every other key | same key under `routing` |

If both blocks are present, `routing:` wins and the legacy block is ignored with a warning. `REPOWIRE_EXPERIMENTS__ROUTER__*` environment variables are migrated the same way. Under a `routing:` block they still apply per field with a deprecation warning, except `MODE` and `ARM`, which are ignored in favour of `routing.strategy`.

A failed arm is recorded by structure only: the fallback reason, the provider, the HTTP status (`http_status`, absent for a transport failure) and `error_code`, the provider's declared machine code if it is on a fixed allowlist of documented values (Anthropic's `error.type` and `details.error_code`, TypeSafe's `detail.error_type`), otherwise `other`. A provider's error body is never stored: it is untrusted and can echo the ask or our credentials. `record_text` governs only the ask text in `route_decision` rows. Every fallback is a miss in replay. Reasons marked "breaker" count toward the provider's breaker (3 consecutive failures open it for 30 s).

| Reason | Meaning | Breaker |
|---|---|---|
| `timeout` | No usable answer before `timeout_seconds` | yes |
| `rate_limited` | Provider returned 429 | yes |
| `billing` | Provider returned 402, or Anthropic's documented spend-limit refusals (400 "You have reached your specified … API usage limits", 429 `enforced_spend_limit_reached`) | yes |
| `unauthorized` | Provider returned 401 or 403 | yes |
| `server_error` | Provider returned 5xx | yes |
| `request_error` | Provider returned another 4xx; usually our request body or template | no |
| `error` | Transport failure with no HTTP status | yes |
| `unparseable` | Answer did not decode, or ranked an ineligible, unknown or repeated candidate | yes |
| `panic` | The arm panicked; contained | yes |
| `no_candidates` | No eligible candidate | no |
| `unconfigured` | Model arm without an API key | no |
| `unknown_arm` | Arm name not registered | no |
| `breaker_open` | Skipped: the provider's breaker is open | no |
| `saturated` | Skipped: the shadow pool was full (shadow only) | no |
| `stale_pick:*` | Targetless asks only: the chosen peer stopped being eligible before delivery | no |

Every key has a `REPOWIRE_ROUTING__*` environment override, for example `REPOWIRE_ROUTING__STRATEGY` or `REPOWIRE_ROUTING__JEV__API_KEY`. `SHADOW_ARMS` is comma-separated.
