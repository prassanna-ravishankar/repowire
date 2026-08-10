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
      gemini: "gemini --yolo"
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

Spawn is disabled until `allowed_paths` and at least one runtime command are configured. `commands` is keyed by backend (`claude-code`, `codex`, `gemini`, `antigravity`, `opencode`, `pi`) and is the single launch profile used by MCP `spawn_peer`, dashboard spawn, backend switching, `repowire peer restart`, and `repowire orchestrator start`.

`profiles` is optional and keyed first by backend, then by a user-defined profile name. Each profile appends structured `args` to the configured backend command; Repowire does not hardcode provider model names. For example, spawning `codex` with profile `fast` runs the configured `daemon.spawn.commands.codex` command plus the profile args. Profile descriptions are informational and may be shown by UIs.

`env_path` and `env` define the environment injected into spawned agent commands.
`env_path` becomes the spawned process `PATH`; `env` adds extra variables such as
tool config paths. If neither `env.PATH` nor `env_path` is set, Repowire captures
the user's login-shell PATH and injects it so launchd/tmux-spawned workers can
still see tools installed by Homebrew, nvm, and similar shell setup. This env is
used by MCP/dashboard spawn, backend switching, restart, and durable job workers.

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
```

`remote_tool_approval` gates `gated_tools` behind a blocking approval question: before a gated tool runs, a `PreToolUse` hook posts the question to the daemon and waits for an allow/deny from a human surface or peer, denying on timeout. The installer only registers the `PreToolUse` hook when `enabled` is set; toggling it off and re-running `repowire setup` removes the hook. Read-only tools are never gated. See [Structured questions](../concepts/message-types.md#pretooluse-tool-approval-claude-code).
