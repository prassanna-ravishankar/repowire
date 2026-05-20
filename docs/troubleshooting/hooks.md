# Hooks not firing

A peer never registers, or stops appearing in `list_peers` after a session restart. Almost always a hook configuration problem.

## Quick check

```bash
repowire status
```

If a runtime is detected but its hooks are marked missing, run `repowire setup` again. Re-running is idempotent and will rewrite the hook entries.

## Per-runtime diagnostics

### Claude Code

1. Open `~/.claude/settings.json`. The `hooks` key should contain entries for `SessionStart`, `UserPromptSubmit`, `Notification`, `Stop`, `StopFailure`, and `SessionEnd`, each pointing at `repowire hook ...`.
2. Confirm `repowire` is on `PATH` for the shell Claude Code was launched from. Hooks shell out, so a missing `repowire` in `PATH` silently no-ops.
3. Start Claude Code in a tmux pane. After your first prompt, run `repowire peer list` in another shell. Peer should appear within a few seconds.

### Codex

1. Confirm `~/.codex/config.toml` contains `[features] hooks = true` (or the legacy `codex_hooks` flag). Without this, hooks don't fire at all.
2. Codex fires `SessionStart` **after** the first user interaction. If your peer never registers, the agent may not have received a first turn — try `notify_peer(peer, "ping")` or send a message manually.
3. If you used `spawn_peer` without a `message`, the seed turn may have been swallowed. Re-send via `notify_peer`.

### Gemini CLI

1. Open `~/.gemini/settings.json`. The `hooks` key should contain `SessionStart`, `BeforeAgent`, and `AfterAgent` entries pointing at `repowire hook ...`.
2. Gemini surfaces hook stderr — if the hook is failing, you'll see it in the Gemini output directly.
3. The `AfterAgent` hook must return `{"decision": "allow"}` to let the agent continue. Repowire emits this by default; if you've patched the hook, make sure that field is still there.

### OpenCode

OpenCode does not use shell hooks. It uses a TypeScript plugin at `~/.opencode/plugin/repowire.ts` (or `.opencode/plugin/` for local installs). If the peer never appears:

1. Confirm the plugin file exists and is non-empty.
2. Check OpenCode's log for plugin load errors.
3. Re-run `repowire setup` to rewrite the plugin file.

## Daemon must be running

All hooks shell out to the daemon over HTTP. If the daemon is down, hooks succeed (they do not block agent startup) but no peer state changes. See [Daemon unreachable](daemon.md).

## After upgrading `repowire`

Hooks call the installed `repowire` binary. If you upgraded via a tool that swapped binaries in place (`uv tool upgrade`, `pipx upgrade`), existing hooks still point at the same path and continue to work. If you reinstalled to a different location, re-run `repowire setup` so the hook entries pick up the new path.
