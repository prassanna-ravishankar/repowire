# Claude Code plugin packaging

This is a packaging spec for a future optional Claude Code marketplace plugin.
It defines what Repowire may bundle through Claude Code's plugin surface without
changing the daemon, setup, or command semantics.

The plugin is a convenience package. `repowire setup` remains the supported
daemon, hook, MCP, service, relay, channel, and multi-runtime install path.

## Goals

- Expose Repowire's mesh command UX contract through Claude Code-native
  packaging where that improves discoverability.
- Keep command semantics identical to the existing CLI, HTTP, MCP, dashboard,
  Telegram, and Slack surfaces.
- Bootstrap the normal `repowire mcp` stdio server when the host already has
  Repowire installed.
- Provide Claude Code-facing docs and skills that teach ask, ack, notify,
  schedule, peer selection, and doctor workflows.
- Detect plugin/package drift before users confuse plugin install success with
  a healthy daemon setup.

## Non-goals

- Do not replace `repowire setup`.
- Do not install or manage the daemon service from the plugin.
- Do not own `~/.repowire/config.yaml`, relay enrollment, auth tokens, or local
  state migration.
- Do not redefine ask/ack/notify lifecycle rules.
- Do not make tracked-work lifecycle, ACP/channel health, SQLite cleanup, or
  graphify claims as part of plugin packaging.
- Do not make Claude Code the only supported runtime. Codex, Gemini CLI,
  OpenCode, Pi, bots, and dashboard surfaces continue to use the shared daemon.

## Proposed plugin layout

The marketplace package should be small and declarative. Runtime code should
stay in the installed `repowire` Python package unless Claude Code requires a
thin wrapper.

```text
repowire-claude-code-plugin/
  .claude-plugin/
    plugin.json
  commands/
    status.md
    peers.md
    pending-asks.md
    ask.md
    notify.md
    schedule.md
    timeline.md
    result.md
    doctor.md
  skills/
    repowire-mesh/SKILL.md
  hooks/
    README.md
  mcp/
    repowire.json
  docs/
    install.md
    update.md
    uninstall.md
    troubleshooting.md
```

### Manifest

`.claude-plugin/plugin.json` should declare only plugin metadata and the
Claude Code integration points that are actually bundled:

- plugin name, description, author, homepage, license, and plugin version
- minimum supported Claude Code version
- compatible Repowire package version range
- command entries that map to the command ids from
  [Mesh command UX contract](mesh-command-ux.md)
- optional skill entries that teach Repowire usage
- optional MCP bootstrap entry for `repowire mcp`
- optional hook documentation or hook config snippets, not a second hook
  implementation

Command names may be Claude Code slash commands, but each must resolve to one
canonical command id: `status`, `peers`, `pending-asks`, `ask`, `notify`,
`schedule`, `timeline`, `result`, or `doctor`.

### Commands

Command files should be prompts or wrappers around the existing surfaces. They
must consume the mesh command UX contract rather than duplicating behavior:

- human output stays compact and scannable
- JSON output uses the shared envelope with `command`, `status`,
  `schema_version`, `data`, optional `target`, `warnings`, and `next_actions`
- routing prefers `peer_id`; display names require `circle` when ambiguous
- `ask` remains non-blocking and returns `correlation_id`
- `notify` remains fire-and-forget
- `ack` remains the only close/reply operation for inbound asks

If a Claude Code command cannot reach the daemon, it should report a setup or
doctor next action. It must not silently create an alternate local mesh.

### MCP bootstrap

The plugin may include a Claude Code MCP entry equivalent to:

```json
{
  "name": "repowire",
  "command": "repowire",
  "args": ["mcp"]
}
```

This bootstrap is valid only when `repowire` is already installed on `PATH`.
If the binary is missing, the plugin should direct the user to install Repowire
and run `repowire setup`. It should not vendor a second daemon or spawn an
unmanaged service.

### Hooks

The plugin may document the hooks installed by `repowire setup` or include
read-only snippets for review. It should not ship a separate hook
implementation unless that implementation is generated from the same installed
Repowire package and covered by the same installer tests.

Default Claude Code integration remains:

- `SessionStart` registers the peer, starts the WebSocket hook supervisor, and
  injects peer context
- `UserPromptSubmit` marks the peer busy
- `Notification` repairs idle status
- `Stop` and `StopFailure` extract chat turns, drain old legacy query FIFO
  responses, fetch pending asks, and repair status
- `SessionEnd` tears down the supervisor and marks the peer offline

Channel mode remains an explicit `repowire setup --experimental-channels`
choice. Plugin packaging may describe it, but must keep it experimental.

## Drift checks

Plugin install success is not proof that Repowire is healthy. The plugin should
surface drift in `doctor` and any install/update docs:

- plugin manifest version vs installed `repowire --version`
- manifest compatible Repowire range vs local package version
- command ids in the manifest vs the command ids in
  `docs/concepts/mesh-command-ux.md`
- MCP bootstrap command vs the installed `repowire mcp` command
- hook snippets or docs vs the installer-owned hook commands
- Claude Code minimum version vs the installed `claude --version`
- channel transport prerequisites only when channel mode is enabled

Version drift should be a warning when commands still resolve and a failure
when the plugin points at missing commands, missing binaries, incompatible MCP
bootstrap, or an unsupported Claude Code version.

## Install docs impact

When Repowire ships a Claude Code marketplace plugin, update these docs
together:

- `README.md`: state that plugin packaging is optional and still requires the
  normal Repowire daemon setup.
- `docs/start/setup.md`: keep `repowire setup` as the canonical machine
  setup and add the plugin as an optional Claude Code discoverability layer.
- `docs/use/features/connect-claude-code.md`: document plugin install, what it bundles, what
  it does not install, and how it relates to hooks, MCP, and channel mode.
- `docs/reference/cli.md`: document any new CLI support used to inspect plugin
  drift, if such a command is added.
- `docs/troubleshooting/diagnostics.md`: include plugin drift checks in the
  doctor explanation.

Do not update uninstall docs to imply plugin removal cleans Repowire state.
Plugin uninstall and Repowire uninstall are separate operations.

## Update docs impact

Plugin update documentation should tell users to:

- update the marketplace plugin through Claude Code
- update the Repowire package through `repowire update` or their package manager
- run `repowire setup` after package updates so hooks and MCP entries are
  rewritten from the installed package
- run `repowire doctor` to catch version, manifest, MCP, and hook drift

The update path should avoid editing daemon state directly.

## Uninstall docs impact

Plugin uninstall documentation should be explicit:

- removing the Claude Code plugin removes packaged slash commands, skills, and
  plugin-provided MCP bootstrap entries
- it does not stop or uninstall the Repowire daemon service
- it does not remove `~/.repowire/`, relay keys, attachments, events, schedules,
  or local state
- users who want to remove Repowire itself should run `repowire uninstall`

If both uninstall paths exist on a machine, docs should recommend removing the
plugin first only when the user no longer wants Claude Code command packaging;
the daemon can continue serving other runtimes.
