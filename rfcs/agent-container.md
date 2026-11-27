# Agent container: is tmux still required?

Status: design, not built (2026-11-24). Asked by Prass: "see if tmux is
still required ... we just use it now as an agent container to handle
spawn/kill."

## Summary

tmux no longer carries messages. Every delivery path goes through a
runtime's own channel: Claude's authenticated inbox via the ws-hook, the
Codex App Server via the bridge, ACP over stdio, and the relay and bots.
What tmux still does is be the container for spawned agents: it places
them, keeps them running across daemon restarts, lets a human attach, and
tells the daemon when they exit. It does the last of these badly, because a
pane is a place rather than a process. Most of the tmux-related fixes in
the last months (#362, #371, #376, #391, #392, #408, #532, #544, #552)
reconstruct "is this peer's agent still the process in this pane" from tmux
formats and the process table.

Proposal: a `Container` socket in the daemon that owns a spawned agent's
execution: reserve a place, launch, bind the runtime when it registers,
stop, watch for its end, attach, and re-adopt after a daemon restart. tmux
becomes one plug, the attachable one, and stays the default. ACP and Codex
App Server threads become plugs for executions nobody needs to watch. The
first slice changes no behaviour: it extracts the socket around today's
tmux code. Exit watching then runs as an observation before it is allowed
to drive offline state.

## 1. What tmux does today

| Job | Where (daemon-go/) | Still needs tmux? |
|---|---|---|
| Spawn: create a pane (new session, new window or split), then `respawn-pane -k` it with the command | `service/spawn_service.go` (`realTmuxController.CreatePane`, `Spawn`, `tmuxStartCommand`, `ensureSession`, `uniqueWindowName`) | Yes for placement. The command line, env and PATH capture (`commandWithEnv`) are not tmux-specific. |
| Kill: `kill-pane` once destructive proof holds | `hub/routes_spawn.go` (`handleKillPeer`, `destructiveProof`), `service/pane_ownership.go`, `hooks/common.go` (`paneSafe`, `findExpectedAgentPID`) | The kill handle is a pane id. The proof is mostly process-table inference: the pane's root pid, the agent pid, its start time, and the command. |
| Restart: reserve a placeholder pane beside the old one, SIGTERM the old runtime and wait for its pid (so it flushes its session file), then for its pane, then launch into the reserved pane; fork starts a sibling and leaves the source running | `hub/peer_restart.go` (`Reserve`, `KillPane`, `ProbePane`), `routes_spawn.go` (`handleForkBackend`) | Yes, through the same controller. The order is safety-critical and must survive any refactor. |
| Exit and liveness: tmux hooks `pane-exited`, `session-closed`, renames, `client-detached`; the ws-hook's `pane_alive` in pong; three strikes in reconcile | `cli/install.go` (`installTmuxLifecycle`), `hub/routes_lifecycle.go`, `hooks/ws.go` (`handleMessage` ping), `peer/reconcile.go` (`demoteUnsafeConnectedPeers`, `runtimeEvidenceIDs`) | Yes. This is where the spurious `session_closed` came from (#376 added the evidence gate). |
| Identity binding: a pane-backed registration must match spawn ownership | `hub/routes_peer_lifecycle.go` (bootstrap), `service/pane_ownership.go`, spawn hints (`writeHint`) | A pane id is the binding key; the spawn hint carries it to late registrants (Codex). |
| Circle binding: the tmux session name or stable window id (`daemon.circle_boundary`) is the circle, and it is enforced: a pane-backed registration whose circle contradicts live tmux evidence is refused | `proto.TmuxCircle`, `hooks/common.go` (`getTmuxInfo`, `tmuxPlacement`), `hub/ws.go`, `hub/routes_peer_lifecycle.go` (bootstrap check), `codexbridge/bridge.go` (`threadTmuxPlacement`, else a fallback or `REPOWIRE_CODEX_CIRCLE`) | For pane-backed peers, yes: tmux is evidence, not just a default. CLI spawn outside tmux needs `--circle`; the Codex bridge has its own fallback. |
| Human attach and observe | Not in Repowire: the human runs `tmux attach` or switches windows. `GET /panes/orphans` and `POST /panes/{id}/link` adopt unknown panes. | Yes, and it is the one job with no replacement today. |
| Survive a daemon restart | tmux's own server; `peer_restart`/startup restore (#371) re-binds panes | Yes. ACP children die with the daemon (`service/acp.go`, `ACPManager.Close`). |
| Message delivery and seeding | Not tmux. The ws-hook delivers to Claude's inbox socket (`hooks/ws.go`, `injectClaudeInbox`); a spawn seed goes through the ordinary ask and notify path (`routes_spawn.go`, `deliverSpawnSeed`) | No. Keystroke injection was removed; no `send-keys` call remains. |
| Trust prompts | Codex hook trust is pre-seeded at install (`cli/install.go`, `trusted_hash`), so it never prompts. Nothing answers a TUI's first-run "trust this folder" prompt: it waits for a human in the pane. | It is a TUI problem, not a tmux one. |
| Logs | None captured; the TUI's scrollback lives in tmux. Transcripts come from hooks (`hooks/streamer.go`) and the Codex bridge. | Only scrollback. |

What is already handled elsewhere: delivery (inbox, App Server, ACP),
transcripts and chat turns (hooks, bridge), seeding, and turn state. Codex
peers in the bridge are threads of Codex's shared App Server, so the pane
only hosts the TUI that a human watches. ACP peers are already
daemon-owned, headless child processes with no pane.

## 2. Who should own an agent's lifecycle

Apply the mantra: the owner of a concern is the layer that holds its data.
An agent's lifecycle is a process: its pid, start time, process group, exit
status, and stdio or pty. Today nothing in Repowire holds the process. tmux
holds a pane, and the daemon infers the process from the pane. That is the
misplaced responsibility behind the proof fixes.

Candidates:

1. **A daemon process supervisor** (pty, process group, pidfd or kqueue).
   It owns the process exactly, but it does not survive
   `repowire service restart` well. Being orphaned does not kill a process
   by itself, but the service manager may clean up the daemon's process
   tree, and the pty or stdio the daemon held goes with it (ACP closes
   stdin and kills its child on purpose: `service/acp.go`). Surviving that
   needs a detached per-agent shim that holds the pty (the containerd-shim
   or dtach pattern), plus an attach client. That is a terminal multiplexer
   we would write and own.
2. **Each backend's native headless mode** (ACP, the Codex App Server, Claude
   stream-json or SDK, `pi --mode rpc`, `opencode serve`). There's no
   terminal at all, and messages arrive over a protocol the daemon already
   speaks (ACP). It's the right home for agents nobody watches. It is not a
   home for TUI work a human wants to watch or steer.
3. **A pluggable `Container` socket** with tmux as one implementation. The
   daemon owns the semantics (handle, proof, exit, restart policy); each
   implementation owns its mechanism.

Recommendation: 3, with tmux and headless as the first two plugs. The
socket is about a runtime execution, not necessarily a process: a Codex
peer is a thread on Codex's shared App Server, with no pid or process group
of its own to watch or kill (the bridge ends it with `thread/interrupt` and
`thread/unsubscribe`, `codexbridge/bridge.go`). So the socket's verbs are
per execution, and only process-shaped plugs map them to signals. A
pty-shim plug stays possible but isn't planned: tmux already is a good
detached pty server with attach, scrollback and survival across daemon
restarts, so rebuilding it would be rebuilding rather than plugging in.

## 3. Semantics

```go
// Container owns spawned agents' executions: where they run, how they end,
// and how a human reaches them. Each plug maps the verbs to its mechanism.
type Container interface {
    Kind() string                                      // "tmux" | "acp" | "codex-thread" | ...
    Reserve(ctx, LaunchSpec) (Placement, error)        // a place to launch into (restart needs it first)
    Launch(ctx, Placement, LaunchSpec) (Handle, error) // starts the runtime in it
    Bind(Handle, RuntimeIdentity) error                // the runtime registered: now we know which process or thread it is
    Stop(ctx, Handle, StopMode) error                  // graceful (SIGTERM, interrupt) or final; proof checked inside
    Watch(Handle, func(Exit))                          // the execution ended; an observation, see order of work
    Attach(Handle) (AttachHint, bool)                  // how a human reaches the live execution, if they can
    Adopt(ctx, Record) (Handle, Adoption)              // after a daemon restart: re-owned, ended meanwhile, or unknown
}

type RuntimeIdentity struct {
    PID       int    // 0 for an execution that is not a process of its own (a Codex thread)
    StartedAt string // the OS start time at its finest resolution, see below
    ThreadID  string // backend-native id, when there is one
}
```

- **Launch does not know the agent.** A tmux launch runs a shell command
  (`respawn-pane -k`); the process there may be a shell, a Node shim or an
  `npx` wrapper that forks the agent and exits. So the handle starts as a
  place, and it learns the runtime at **Bind**, when the runtime registers
  through its own hook or bridge. Hooks already report `agent_pid` with its
  start time (#544), and the bridge reports the thread. A wrapper that uses
  `exec` keeps the pid; one that forks is covered because the registering
  hook names the real process.
- **Stop keeps today's proof.** For tmux, a kill still requires the agent to
  be running inside the proven pane (`routes_spawn.go`, `PaneRunsPeer`), on
  the live tmux server (#408), and it still removes the pane with
  `kill-pane`. It never signals a numeric process group it did not create.
  A daemon-child plug (ACP) creates its own process group at launch
  (`Setpgid`) and signals only that. A thread plug interrupts and
  unsubscribes its own thread and never touches the shared server. Restart
  keeps its order: reserve, SIGTERM and wait for the pid, then the pane,
  then launch into the reservation.
- **A Codex TUI peer has two lifetimes.** The TUI runs in a tmux pane,
  while the peer is a thread on the App Server, and the bridge registers it
  with a thread id and no agent pid (`codexbridge/bridge.go`;
  `peer_restart.go` already handles the missing pid). Its handle is the
  tmux placement plus the thread. The pane's lifetime is the TUI's, and the
  thread's lifetime is the peer's. Until the codex-thread plug exists (step
  4), these peers have no bound pid and stay on today's paths, so step 3
  does not touch them. Restart relaunches the TUI into a reserved pane as
  now, and the bridge moves the thread to it by the spawn hint.
- **Watch** is the process exit where there is a process: `pidfd_open` on
  Linux 5.3+ and kqueue `EVFILT_PROC`/`NOTE_EXIT` on macOS, both usable for
  a non-child pid of the same user. Exit status is not part of the
  contract: Linux `waitid` and macOS `NOTE_EXITSTATUS` give it only for a
  child. For a thread, it's the bridge's thread-closed event.
- **Identity across restarts.** `ps -o lstart` has one-second resolution,
  which is not a recycle-proof identity on its own. Records should carry the
  kernel start time at its native resolution (Linux `/proc/<pid>/stat`
  field 22 in clock ticks, macOS `kinfo_proc.p_starttime` in microseconds)
  together with the pid.
- **Adopt** reconciles after a restart rather than just re-arming watches.
  It reads each record and finds the process with that pid and start time:
  alive (re-arm the watch, re-checking identity after subscribing, so an
  exit between check and arm is not missed), gone (the execution ended while
  the daemon was down: offline with that reason), or unknown (watch error or
  no permission: fall back to today's lazy probe). A callback carries the
  record's runtime generation, so a late callback for a replaced runtime is
  dropped.
- **Attach** for tmux is `switch-client -t <session>` when a client is
  attached, otherwise `attach -t =<session>`, then `select-window` and
  `select-pane` (selecting a pane alone can leave its window hidden). For
  other plugs it is "none" unless a backend can attach to the live execution
  itself. Resuming a session is a different thing (it can start a second
  writer) and is not offered as attach. The dashboard's transcript stays the
  universal way to observe.
- **Restart policy** stays an explicit action (`restart_peer`). A container
  never restarts an agent on its own.

### Plugs

The socket is designed to fit every execution Repowire launches today or has
planned. Each plug maps the verbs to its own mechanism, and a verb a plug
cannot honour is declared unsupported, not faked.

| Verb | tmux (default) | ACP (daemon child) | Codex thread (App Server) | Hosted (#517, Cloudflare; dropped 2026-11-27) |
|---|---|---|---|---|
| Reserve | placeholder pane | none | none | none |
| Launch | `respawn-pane -k` with the command | spawn with its own process group, lazily on first prompt (today) | `thread/start` through the bridge | `Hosted.Spawn`: allocate, then `CreateAgent` (a reclaimed peer reattaches to its agent and conversation) |
| Bind | the hook's `agent_pid` and start time | the child pid | the thread id | the hosted agent id, which is the peer id |
| Stop | proof, SIGTERM or `kill-pane` | signal its own group, close stdio | `thread/interrupt`, `thread/unsubscribe` | `Hosted.Retire`: `RetireAgent` (a 404 counts as done), then `MarkOffline(retire)`: a soft retirement, final for input; the transcript stays. The Worker's `/abort` (one operation or all) is the non-final control, and the daemon does not expose it |
| Watch | pid exit (pidfd or kqueue) | child exit | the bridge's thread-closed event | none: no process. Inbound `Submit` fails loudly if the runtime refuses, reports replay from the Worker outbox, and the gateway socket gates only the agent's outbound mesh tools |
| Sleep and wake | none | none | none | the Worker's own concern (idle, working, parked, failed and retired live in its Gateway). The daemon sees only addressable plus busy: BUSY while a submission is in flight |
| Attach | `switch-client` or `attach`, then window and pane | none | to verify (open question) | none; the transcript is observe-only |
| Adopt | pid and start time reconcile | none: does not survive a daemon restart | the bridge resubscribes | `Hosted.Restore`: mappings, plus unsettled operations resubmitted by id |

The hosted column is the shape #517 had until 2026-11-27, when the daemon's
hosted backend was dropped: managed agents are now reached only through the
relay, as the `managed` machine, so no daemon plug owns them.

Two consequences for the socket's shape:
- **Watch is optional.** For a plug without a process (hosted), "ended"
  is only what the plug reports (a retire, or a refusal), and the daemon
  never infers death from silence.
- **Plugs that own peers outright** (hosted) implement `service.PeerContainer`. `kill_peer` asks each one whether it `Owns` the peer. The first that does stops it with `StopPeer` (no pane proof, no registry change), and the caller then applies the same operator close as for every backend: `ClosePeer`, then unregister. A failed `StopPeer` answers `kill_failed` and keeps the peer (#584).
- **Sleeping is not ending.** A plug reports at most "addressable" and
  "busy". A plug may report an execution as addressable but not running. The registry must keep such a peer
  reachable, not offline. The hosted backend already does this by routing
  submissions durably, as for the human inbox.

### What moves

- `TmuxController` becomes the tmux plug, and `PaneOwnership` becomes
  ownership records keyed by peer id that carry the placement and the bound
  runtime identity.
- `ACPManager`'s process ownership becomes the ACP plug, and the Codex
  bridge's thread teardown becomes the codex-thread plug. The bridge keeps
  its protocol work.
- The consequences of a death stay in one place, whichever signal is
  authoritative: forget ownership, clear runtime metadata, disconnect the
  transport, and cancel asks (today `HandlePaneDied`,
  `hub/routes_lifecycle.go`), and retire identity on a proven death (today
  reconcile's three strikes, `peer/reconcile.go`).
- Peers a human started by hand are not containerized. They keep today's
  hooks, pane-death and three-strike paths unchanged.

### What we lose without tmux (for non-tmux plugs only)

- **Human attach:** a live TUI and its scrollback.
- **Prompts:** interactive permission and trust prompts. A headless runtime
  needs an explicit approval policy instead.
- **Daemon-restart survival:** stated per backend.
  - ACP over stdio does not survive: the daemon owns the pipe, and a pid
    watch cannot restore a protocol connection.
  - A Codex thread survives because the App Server is Codex's own and the
    bridge resubscribes.
  - `opencode serve` would survive the same way.
  - Claude stream-json under the daemon would not.

### Which tmux pains go away

- **Spurious `session_closed`:** once the exit watch is authoritative for
  spawned peers (step 3), a session event no longer offlines them. Until
  then it is unchanged, and the #376 evidence gate stays for hand-started
  peers.
- **`kill_peer` leaving windows or shells:** `Stop` owns the pane it
  reserved and removes it. tmux closes the window itself when its last pane
  goes, so a window shared since with a human's or another agent's pane is
  never removed. Restart's leftover-shell cleanup moves into the plug.
- **Trust prompts:** gone for non-tmux plugs (non-interactive). They remain
  for TUIs, where a human is present by design.
- **Pane-proof inference** (#532, #544, #552): the bound runtime identity
  replaces "find the agent under the pane's shell" for spawned peers, though
  tmux kills keep the containment check as a second gate.

## 4. Order of work

1. **Extract the socket, with no behaviour change.** `TmuxController` and
   ownership move behind `Container` and records gain the bound runtime
   identity. Every decision stays on today's paths. Tests prove the
   restart order, the kill proof and pane-death consequences are unchanged.
2. **Watch as an observation.** Arm exit watches for spawned peers at Bind
   and at Adopt, and record when a watch and a tmux signal disagree (a
   contradiction event), without acting on the watch. This measures whether
   the watch would have avoided the spurious offlines before anything
   depends on it.
3. **Switch authority for spawned peers with a bound pid.** The exit
   watch drives offline state and death consequences. For those peers,
   `session_closed`, `pane-exited` and reconcile's three `pane_alive=false`
   strikes (`peer/reconcile.go`, `demoteUnsafeConnectedPeers`) become
   hints, recorded but not acted on. Otherwise the strikes could still
   retire a peer whose watch says it is alive. Hand-started peers and
   peers without a bound pid keep today's paths.
4. **The ACP plug,** then the **codex-thread plug,** each with its
   restart-survival statement.
5. **Optional:** `repowire attach <peer>` over every plug that can attach.

The smallest first slice is step 1. It is the precondition for everything
else and changes nothing a user sees. Step 2 is the smallest slice that
produces evidence.

## Claude Code plug options

Claude Code is the main backend still in tmux. tmux was needed because most
agents had no client/server mode. Codex (App Server), ACP agents and Pi now
have one. This section records what Claude Code documents today (checked
2026-11-24) and whether a non-tmux plug with acceptable observability looks
feasible. Sources are the official docs at code.claude.com.

**What exists**

- **Long-lived headless process.**
  - `claude -p --input-format stream-json --output-format stream-json`
    can take several user messages on one process: a message still
    queued when a turn ends starts a new turn. `--replay-user-messages`
    echoes stdin turns on stdout. Sources: [CLI reference], [headless].
  - The exact stdin message schema is not documented in those pages.
- **Agent SDK** (TypeScript and Python) "runs the Claude Code binary"
  ([Agent SDK overview]).
  - Streaming input is an async generator in TypeScript and
    `ClaudeSDKClient.query()` in Python.
  - `interrupt()` ends the current response and keeps the session open.
    `setPermissionMode()` and `close()` are also available.
  - `canUseTool` approves or denies each tool call that no rule already
    decided, and it may stay pending indefinitely.
  - Sources: [streaming input], [SDK permissions], [TypeScript SDK].
- **Permissions without a TUI.** These are documented for `-p`:
  `--permission-mode`, `--allowedTools`, `--permission-prompt-tool` (routes
  prompts to an MCP tool), and `--permission-prompts none`. The workspace
  trust dialog is not shown in `-p` or SDK sessions ([headless],
  [permissions]).
- **Hooks and MCP.** Hook events fire wherever Claude Code runs. Project
  hooks run under `-p` unless `--bare` is used. `--mcp-config` loads MCP
  servers in `-p` ([hooks], [headless]). Whether `Notification` fires under
  `-p` is not stated.
- **Continuity.**
  - `--resume <id>`, `--continue` and `--fork-session` are documented, and
    the SDK lets the caller choose the session id.
  - A session created by `-p` or the SDK can later be opened interactively
    with `claude --resume <id>`.
  - Two processes writing the same session can lose transcript messages,
    so the docs advise `--fork-session` for each.
  - Source: [sessions].
- **Live observation.**
  - stream-json carries token deltas (`--include-partial-messages`), hook
    events (`--include-hook-events`), `system/init`, retries and
    permission denials ([headless]).
  - Transcripts are JSONL under `~/.claude/projects/`. Their entry format
    is internal and may change ([sessions]).
- **Background sessions with attach** (research preview).
  - `claude --bg` runs a session under Claude Code's own supervisor.
  - `claude attach <id>` opens a full interactive TUI that a human can
    type into, and detaching leaves it running.
  - `claude logs`, `claude stop` and `claude agents --json` (the
    supported way to read state from outside) round it out.
  - No control API or socket for an external daemon is documented
    ([agent view]).
- **Remote Control.** `claude remote-control` is a server mode reached
  through Anthropic's service: outbound HTTPS only, and a claude.ai login is
  required. Its use with `-p` or SDK sessions is not documented
  ([remote control]).
- **The inbox socket** Repowire uses today for delivery is not covered in
  the public docs.

**Assessment**

- A daemon-hosted Claude Code plug is feasible with documented features.
  - One long-lived stream-json process (or SDK client) per agent can take
    multiple turns, be interrupted, and resume by id.
  - Its permissions can go through `canUseTool` or a permission-prompt
    MCP tool, the human's approval surfaces included.
  - Its exit is a child exit, like ACP.
  - stream-json gives the dashboard a live view, and hook events still
    fire.
- The gap is a human taking over a live session. The daemon would own the
  stream, and `claude attach` only reaches sessions under Claude Code's own
  supervisor. Two writers on one session can lose messages, so takeover
  would be stop, then `claude --resume <id>` in a TUI, then hand back by
  resuming headless. That handoff is not documented as a supported flow.
- Two shapes follow:
  - **(a) SDK or stream-json plug.** Observe-only live, with takeover by
    stop-and-resume. It's the right fit for agents nobody steers.
  - **(b) `claude --bg` as the plug.** Live attach comes for free, but
    `claude agents --json` is its only state interface, and it is a
    research preview.
- Recommendation: (a) for spawned workers once the ACP plug exists, and
  keep tmux for Claude Code sessions a human wants to steer. Revisit (b) if
  Claude Code documents a control interface for background sessions.

[CLI reference]: https://code.claude.com/docs/en/cli-reference.md
[headless]: https://code.claude.com/docs/en/headless.md
[Agent SDK overview]: https://code.claude.com/docs/en/agent-sdk/overview.md
[streaming input]: https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode.md
[SDK permissions]: https://code.claude.com/docs/en/agent-sdk/permissions.md
[TypeScript SDK]: https://code.claude.com/docs/en/agent-sdk/typescript.md
[permissions]: https://code.claude.com/docs/en/permissions.md
[hooks]: https://code.claude.com/docs/en/hooks.md
[sessions]: https://code.claude.com/docs/en/sessions.md
[agent view]: https://code.claude.com/docs/en/agent-view.md
[remote control]: https://code.claude.com/docs/en/remote-control.md

## Open questions

- Linux without pidfd (before 5.3): fall back to the lazy probe on request,
  never a timer?
- Can a human TUI attach to a live Codex App Server thread without becoming
  a second writer? Verify on codex-cli 0.171 before the codex-thread plug
  offers attach.
- Should a non-tmux spawn default to its spawner's circle, or always
  require one?

## Review

Codex (headless `codex exec`, read-only) on the first draft raised three
P1s, all taken in:
- the launched pid is not the agent (so the handle binds at registration);
- a pid with a start time does not prove a pane or a process group (so
  tmux keeps the containment check and nothing signals a group it did not
  create);
- a Codex thread is not a process (so the verbs are per execution).

Its P2s are also in:
- adoption reconciles rather than just re-arming watches;
- exit status is optional;
- the start-time resolution is now specified;
- the first slice is extraction only;
- restart's order is spelled out;
- circle binding is evidence, not just a default;
- survival is stated per backend.

Its P3, on attach, is also in.

The second round found no P1s. Its three P2s are also in:
- a Codex TUI peer's two lifetimes;
- reconcile's three strikes are gated when authority switches;
- tmux, not the container, closes a window, so a shared window is never removed.
