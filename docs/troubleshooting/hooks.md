# Hooks not firing

A peer never registers, or stops appearing in `list_peers` after a session restart. Almost always a hook configuration problem.

## Quick check

```bash
repowire status
repowire doctor
```

If a runtime is detected but its hooks are marked missing, run `repowire setup` again. Re-running is idempotent and will rewrite the hook entries.

`repowire doctor` also checks that the daemon, `tmux`, and `git` are available.
Re-run `repowire setup` to reinstall both runtime integrations and the
server-global tmux lifecycle hooks used for pane exits and renames.

## Per-runtime diagnostics

### Claude Code

1. Open `~/.claude/settings.json`. The `hooks` key should contain entries for `SessionStart`, `UserPromptSubmit`, `Notification`, `Stop`, `StopFailure`, and `SessionEnd`, each pointing at `repowire hook ...`.
2. Confirm `repowire` is on `PATH` for the shell Claude Code was launched from. Hooks shell out, so a missing `repowire` in `PATH` silently no-ops.
3. Start Claude Code in a tmux pane. After your first prompt, run `repowire peer list` in another shell. Peer should appear within a few seconds.
4. On Claude Code 2.1.224+, `/status` should show a `Peer address` and `repowire peer describe NAME` should report `transport: claude-inbox`. If native delivery fails, Repowire logs the socket error under `~/.cache/repowire/logs/ws-hook-*.log` and returns a failed delivery receipt. There is no keystroke fallback.

### Codex

Current Codex releases use App Server for registration, lifecycle, chat, and
delivery. They retain only a reminder-only Stop hook so unacknowledged asks
resurface after a turn. Check `repowire service status`,
`~/.repowire/codex-bridge.log`, and the `Stop` entry in
`~/.codex/hooks.json`. Older Codex releases without `app-server --listen` retain
the full hooks transport.

### OpenCode

OpenCode does not use shell hooks. It uses a TypeScript plugin at `~/.config/opencode/plugins/repowire.ts`. If the peer never appears:

1. Confirm the plugin file exists and is non-empty.
2. Check OpenCode's log for plugin load errors.
3. Re-run `repowire setup` to rewrite the plugin file.

## Daemon must be running

All hooks shell out to the daemon over HTTP. If the daemon is down, hooks succeed (they do not block agent startup) but no peer state changes. See [Daemon unreachable](daemon.md).

## After upgrading `repowire`

Hooks call the installed `repowire` binary. Native upgrades keep the `~/.local/bin/repowire` symlink stable. If you install to a different location, re-run `repowire setup` so hook entries pick up the new path.
