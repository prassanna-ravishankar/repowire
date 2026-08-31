# Codex

OpenAI's Codex CLI connects through its native App Server thread API. Repowire
keeps the normal Codex TUI visible; App Server replaces tmux keystroke injection
as the message transport.

## What gets installed

| Surface | Purpose |
| --- | --- |
| `repowire-codex` user service | Runs the local Codex App Server and thread bridge |
| `~/.codex/config.toml` | Installs the Repowire MCP tools |
| `~/.codex/hooks.json` | A reminder-only Stop hook keeps unacknowledged asks visible |

The MCP entry points at the installed `repowire` binary:

```toml
[mcp_servers.repowire]
command = "repowire"
args = ["mcp"]

[mcp_servers.repowire.env]
REPOWIRE_BACKEND = "codex"
```

Older Codex releases without `app-server --listen` retain the hooks transport.

## Registration and delivery

`repowire setup` installs an independently supervised Codex companion. It starts
`codex app-server --listen unix://`; plain `codex`, `codex resume`, and the normal
TUI automatically use that local control socket.

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
turn. Inbound peer content keeps its `<peer-message>` provenance and ask
correlation id; dashboard, Telegram, and Slack messages remain direct human
instructions. Uploaded images are also passed as native Codex image input when
they resolve to a daemon-owned attachment file. Other attachments remain
visible as text metadata.

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

The App Server companion is separate from the Repowire daemon. `repowire service
restart` restarts routing without killing Codex threads. On macOS, restarting
the bridge also preserves the independent App Server. `repowire service stop`
or `uninstall` stops both services.

On macOS the independent App Server uses Codex's stored login. Custom model
providers that rely on an `env_key` must expose that variable to the launchd
user domain (for example with `launchctl setenv KEY VALUE` before service
installation); Repowire does not copy secrets into its plist. The legacy
bridge-owned fallback on other platforms still takes a bounded login-shell
snapshot and forwards only provider variables named in Codex config.

## Verifying

```bash
repowire service status
codex
# in another terminal, before prompting Codex:
repowire peer list
```

The Codex peer should already be listed. Its metadata reports
`transport=codex-app-server`, and its TUI remains interactive.

### macOS process ownership

Repowire setup installs the official signed native Codex App Server as the user LaunchAgent `io.repowire.codex-app-server`. The Repowire bridge is a separate client of its Unix socket, not its parent. This matters for macOS privacy controls: tools launched by Codex are attributed to Codex instead of to the Repowire executable. Routine Repowire daemon and bridge restarts preserve the App Server and its live threads. Existing installations have one unavoidable process restart when they first migrate to this layout.

## Troubleshooting

- Codex peer never registers → run `repowire service status`, then inspect
  `~/.repowire/codex-bridge.log` and `~/.repowire/codex-app-server.log`.
- Codex joins `default` instead of a tmux circle → more than one Codex tmux
  circle matched the same working directory, or none did. Spawn it through
  Repowire for an explicit circle.
- MCP tools return errors → check `~/.codex/config.toml` contains
  `[mcp_servers.repowire]` and that `repowire` is on the service `PATH`.
