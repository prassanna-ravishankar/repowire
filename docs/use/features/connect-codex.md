# Codex

OpenAI's Codex CLI connects through its native App Server thread API. Repowire
keeps the normal Codex TUI visible; App Server replaces tmux keystroke injection
as the message transport.

## What gets installed

| Surface | Purpose |
| --- | --- |
| `repowire-codex` user service | Runs the thread bridge, which attaches to Codex's shared App Server |
| `~/.codex/config.toml` | Installs the Repowire MCP tools |
| `~/.codex/hooks.json` | A reminder-only Stop hook keeps unacknowledged asks visible |

The MCP entry points at the installed `repowire` binary:

```toml
[mcp_servers.repowire]
command = "repowire"
args = ["mcp"]

[mcp_servers.repowire.env]
REPOWIRE_BACKEND = "codex"

[mcp_servers.repowire.tools.ack]
approval_mode = "approve"

[mcp_servers.repowire.tools.answer]
approval_mode = "approve"

[mcp_servers.repowire.tools.decline]
approval_mode = "approve"
```

Setup pre-approves the ask-resolution tools so a non-interactive approval
policy cannot leave the Stop hook permanently blocked on an ask it cannot close.

Codex releases without `daemon_auto_start` (before 0.158, or with the feature
disabled) retain the hooks transport.

## Registration and delivery

Codex owns its App Server. Since 0.158 every Codex TUI starts a shared
background server if none is running (`daemon_auto_start`) and attaches to it,
and Codex restarts that server across its own upgrades. `repowire setup`
installs only the bridge, which attaches to the same control socket
(`$CODEX_HOME/app-server-control/app-server-control.sock`) and waits while no
server is running. Sessions started with `codex --no-daemon`, or that choose
"Run without daemon" at startup, use a private embedded server and stay off the
mesh.

A thread registers as soon as Codex creates it, before its first user prompt.
There is no warmup prompt or `UserPromptSubmit` one-turn delay. Repowire sends an
idle thread a native `turn/start` request and steers an active thread with
`turn/steer`. App Server lifecycle notifications drive `busy` and `online`
status, including interrupt and completion boundaries. A thread resumed after
the bridge starts is discovered from its first status notification, so service
restarts do not leave its MCP calls under the fallback `mcp-http` identity.
Because completed-item
notifications are scoped to the App Server client that started the turn, the
bridge reads the completed turn once when the thread becomes idle; it does not
poll.

The bridge injects the mesh identity, peer list, ask/ack conventions, and saved
handoff directly into the thread's model-visible history without starting a
turn. It also injects a model-only description reminder before the first turn
and after each completed turn for the next prompt. The reminder shows the
current value, asks Codex to update it when the task changes, and explicitly
calls for `set_description` when no description is set. Inbound peer content
keeps its `<peer-message>` provenance and ask correlation id; dashboard,
Telegram, and Slack messages remain direct human instructions. Uploaded images
are also passed as native Codex image input when they resolve to a daemon-owned
attachment file. Other attachments remain visible as text metadata.

App Server shares one MCP subprocess across threads, so Codex includes the
calling thread as `_meta.threadId` on each tool call. Repowire uses that id only
to locate the daemon-minted runtime certificate saved by the bridge, then
validates the certificate before assigning the call to the peer. The MCP shim
therefore uses the same `peer_id` as the App Server thread instead of lazily
creating a second peer. `CODEX_THREAD_ID` remains a fallback for Codex surfaces
that launch MCP per thread.

The Stop hook remains as a narrow reliability backstop: if Codex completes a
turn without acknowledging an open ask, it blocks with a reminder. It does
not register the peer, report status or chat, or deliver messages; App Server
owns those paths.

Tmux remains useful for hosting and restarting a TUI, but it is not used for
message delivery. When exactly one tmux circle matches a Codex thread's working
directory, Repowire preserves that session/window circle even when several
panes in that circle share the path, without binding the peer to a pane. Spawn
hints take precedence. A standalone thread with no safe
placement evidence joins the explicit `default` circle.

Final App Server chat events include completed command, file-change, MCP, and
other supported tool-call summaries for the dashboard. The bridge also saves
the latest completed turn as handoff context. Registration metadata includes
branch and git status, plus tmux diagnostics only when exactly one matching
Codex pane can be identified; ambiguous cwd matches are deliberately omitted.

The bridge is separate from the Repowire daemon, and neither owns the App
Server. `repowire service restart`, `restart bridge`, `stop`, and `uninstall`
never stop Codex threads. The App Server runs with the environment of the Codex
TUI that started it, so custom model providers that rely on an `env_key` work
as they do in plain Codex.

## Verifying

```bash
repowire service status
codex
# in another terminal, before prompting Codex:
repowire peer list
```

The Codex peer should already be listed. Its metadata reports
`transport=codex-app-server`, and its TUI remains interactive.

### Process ownership

The App Server is started by Codex from your terminal, so macOS attributes
privacy prompts for Codex's tools to the terminal, exactly as in plain Codex.
Repowire never launches it.

Releases before this change ran their own App Server as the LaunchAgent
`io.repowire.codex-app-server`. Codex did not manage or upgrade that server, so
after a Codex upgrade new TUIs reported "Background server has incompatible
feature settings". Setup now removes that LaunchAgent and runs
`codex app-server daemon start`; open Codex TUIs reconnect to the new server
with their conversations intact.

## Troubleshooting

- Codex peer never registers → run `repowire service status`, then inspect
  `~/.repowire/codex-bridge.log`. `codex app-server daemon version` shows
  whether Codex's shared server is running and which version it serves.
- Codex joins `default` instead of a tmux circle → more than one Codex tmux
  circle matched the same working directory, or none did. Spawn it through
  Repowire for an explicit circle.
- MCP tools return errors → check `~/.codex/config.toml` contains
  `[mcp_servers.repowire]` and that `repowire` is on the service `PATH`.
