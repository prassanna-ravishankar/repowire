# Antigravity CLI (`agy`)

Google's [Antigravity](https://antigravity.google) CLI, distinct from Gemini CLI. Separate binary (`agy`), separate auth, separate app-data directory (`~/.gemini/antigravity-cli/`).

## What gets installed

Repowire ships a plugin into the Antigravity plugins directory at `~/.gemini/antigravity-cli/plugins/repowire/` and registers it in `import_manifest.json`. The plugin layout has been verified with `agy plugin validate`:

```
~/.gemini/antigravity-cli/plugins/repowire/
├── plugin.json          # {"name": "repowire", "version": "...", ...}
└── hooks/
    └── hooks.json       # SessionStart / BeforeAgent / AfterAgent
```

The validated hook file uses Gemini-style event names. If `agy` fires those plugin hooks, Repowire's adapter at `hooks/adapters.py` maps `BeforeAgent`/`AfterAgent` to canonical prompt/stop events.

| Antigravity event | Mapped to | Command |
| --- | --- | --- |
| `SessionStart` (matcher: `startup`) | session hook | `repowire hook session --backend=antigravity` |
| `BeforeAgent` | prompt hook | `repowire hook prompt --backend=antigravity` |
| `AfterAgent` | stop hook | `repowire hook stop --backend=antigravity` |

## Pending upstream verification

The Antigravity CLI plugin layout validates cleanly, but two pieces of end-to-end behaviour are not yet confirmed and should be treated as best-effort:

1. **Whether `agy` actually fires plugin-defined hooks at session boundaries today.** `agy --help` does not document the hook system, and the plugin validator reports `hooks: 1 processed` without running them. `repowire status` and `repowire doctor` mark this gap as a `WARN`, not an `OK`.
2. **MCP server registration via the plugin system.** The `mcpServers/` plugin subdirectory shape couldn't be verified through `agy plugin validate`. The installer deliberately does **not** write any MCP entries — writing unknown shapes is worse than an honest gap. `check_mcp_installed()` always returns `False` for Antigravity until the schema is documented or observed.

When upstream lands hook/MCP support, the installer will be tightened to write the verified shapes and `doctor` will flip from `WARN` to `OK` without changes to the surrounding integration.

## CLI fallback (while hooks are pending)

Until `agy` fires plugin hooks, an agy session can still participate in the mesh by shelling out to the daemon over HTTP. Three commands cover the lifecycle:

```bash
# 1. Self-register the running agy session as a peer (one-time per session).
repowire peer whoami --register --backend antigravity \
    --name agy-demo --circle default

# 2. List inbound asks for the current pane (defaults to $TMUX_PANE).
repowire peer asks

# 3. Close an ask, optionally with a reply.
repowire peer ack ask-abc123 -m "pong"
```

All three resolve identity in this order: explicit `--peer-id` / `--pane-id`, `$TMUX_PANE`, then `--peer NAME`. `peer whoami` (no flags) is a read-only check that prints the registered identity for the current pane. See [CLI reference](../../reference/cli.md) for full flag listings.

## Spawn

Daemon/MCP spawn pre-registers Antigravity peers as CLI-fallback peers because the `SessionStart` hook is not reliable yet. `spawn_peer(..., backend="antigravity")` returns the `peer_id`, `registration_state=cli_fallback`, and a warning. The peer appears in `list_peers` immediately, and `ask` / `notify_peer` delivery queues for `repowire peer asks` / `repowire peer deliveries` until upstream hook firing is verified.

`repowire peer new --backend antigravity` uses the same daemon spawn path. The deprecated `peer new --command ...` escape hatch launches tmux directly and does not pre-register; run `repowire peer whoami --register --backend antigravity` inside that pane if you use the direct command override.

Default spawn command: `agy --dangerously-skip-permissions`.

```yaml
daemon:
  spawn:
    commands:
      antigravity: "agy --dangerously-skip-permissions"
```

## Verifying

```bash
repowire status     # ⚠ antigravity (plugin installed; hook firing pending upstream)
repowire doctor     # WARN: vX.Y.Z — plugin installed; hook firing/MCP pending upstream
agy plugin list     # should show repowire under "imports"
```

For a repeatable evidence snapshot, run the smoke harness:

```bash
python3 scripts/agy_interop_smoke.py --run-cli-fallback --output agy-smoke.json
```

The JSON report records current local evidence separately for the `agy` binary,
plugin layout, daemon-observed hook firing, Antigravity MCP availability, and
CLI fallback ask→ack. The fallback smoke creates temporary peer identities and
deletes them before exiting. A `not_observed` or `not_verified` status is an
honest current-state observation, not a claim that upstream support can never
work.

## Uninstall

`repowire uninstall` removes the plugin directory and manifest entry. Equivalent manual cleanup:

```bash
agy plugin uninstall repowire
```
