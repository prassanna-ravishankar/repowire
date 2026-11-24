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

Proposal: a `Container` socket in the daemon that owns an agent's process
lifecycle (launch, kill, exit, attach, adopt after a daemon restart). tmux
becomes one implementation, the attachable one, and stays the default. A
headless implementation plugs in for backends with a native headless
protocol. The first slice changes no behaviour: it extracts the socket
around today's tmux code and moves exit detection from tmux hooks to the
agent's own process.

## 1. What tmux does today

| Job | Where (daemon-go/) | Still needs tmux? |
|---|---|---|
| Spawn: create a pane (new session, new window or split), then `respawn-pane -k` it with the command | `service/spawn_service.go` (`realTmuxController.CreatePane`, `Spawn`, `tmuxStartCommand`, `ensureSession`, `uniqueWindowName`) | Yes for placement. The command line, env and PATH capture (`commandWithEnv`) are not tmux-specific. |
| Kill: `kill-pane` once destructive proof holds | `hub/routes_spawn.go` (`handleKillPeer`, `destructiveProof`), `service/pane_ownership.go`, `hooks/common.go` (`paneSafe`, `findExpectedAgentPID`) | The kill handle is a pane id. The proof is mostly process-table inference: the pane's root pid, the agent pid, its start time, and the command. |
| Restart and fork: reserve a pane, launch, kill the old pane | `hub/peer_restart.go` (`KillPane`, `ProbePane`, reserved panes), `routes_spawn.go` fork | Yes, through the same controller. |
| Exit and liveness: tmux hooks `pane-exited`, `session-closed`, renames, `client-detached`; the ws-hook's `pane_alive` in pong; three strikes in reconcile | `cli/install.go` (`installTmuxLifecycle`), `hub/routes_lifecycle.go`, `hooks/ws.go` (`handleMessage` ping), `peer/reconcile.go` (`demoteUnsafeConnectedPeers`, `runtimeEvidenceIDs`) | Yes. This is where the spurious `session_closed` came from (#376 added the evidence gate). |
| Identity binding: a pane-backed registration must match spawn ownership | `hub/routes_peer_lifecycle.go` (bootstrap), `service/pane_ownership.go`, spawn hints (`writeHint`) | A pane id is the binding key; the spawn hint carries it to late registrants (Codex). |
| Circle default: the tmux session or window names the circle | `proto.TmuxCircle`, `hooks/common.go` (`getTmuxInfo`, `tmuxPlacement`), `hub/ws.go`, `codexbridge/bridge.go` (`threadTmuxPlacement` via `list-panes`) | Only as a default; outside tmux, `--circle` is already required. |
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
   It owns the process exactly, but a child of the daemon dies or is
   orphaned on `repowire service restart`. Surviving that needs a detached
   per-agent shim that holds the pty (the containerd-shim or dtach pattern),
   plus an attach client. That is a terminal multiplexer we would write and
   own.
2. **Each backend's native headless mode** (ACP, the Codex App Server, Claude
   stream-json or SDK, `pi --mode rpc`, `opencode serve`). There's no
   terminal at all, and messages arrive over a protocol the daemon already
   speaks (ACP). It's the right home for agents nobody watches. It is not a
   home for TUI work a human wants to watch or steer.
3. **A pluggable `Container` socket** with tmux as one implementation. The
   daemon owns the semantics (handle, proof, exit, restart policy); each
   implementation owns its mechanism.

Recommendation: 3, with tmux and headless as the first two plugs. A
pty-shim plug stays possible but isn't planned: tmux already is a good
detached pty server with attach, scrollback and survival across daemon
restarts, so rebuilding it would be rebuilding rather than plugging in.

## 3. Semantics

```go
// Container launches and owns spawned agents' processes.
type Container interface {
    Kind() string                                  // "tmux" | "headless"
    Launch(ctx, LaunchSpec) (Handle, error)        // places and starts the agent
    Kill(ctx, Handle) error                        // the whole process group; the place goes too
    Watch(Handle, func(Exit))                      // exit of the agent process itself
    Attach(Handle) (AttachHint, bool)              // how a human gets to it, if they can
    Adopt(ctx, Record) (Handle, bool)              // re-own after a daemon restart
}

type Handle struct {
    ID        string    // container-scoped id (tmux pane id, headless run id)
    PID       int       // the agent process, not a shell
    StartedAt time.Time // with PID, the recycle-proof identity (#544)
    PGID      int
    Place     map[string]string // tmux: session, window, pane; headless: none
}
```

- **Launch** receives the resolved command, env, cwd, circle, peer id and
  seed intent (today's `SpawnConfig`). It returns once the agent process
  exists; registration still happens through the runtime's own hook or
  bridge.
- **Kill** proves ownership by the handle, not by inference: the durable
  ownership record stores `{container, id, pid, started_at}`, and a kill
  checks that this pid with this start time is alive before signalling
  its process group. The path is still never proof.
- **Exit** comes from the process: `pidfd_open` on Linux and kqueue
  `EVFILT_PROC/NOTE_EXIT` on macOS. Both work for a non-child pid owned by
  the same user, so they survive a daemon restart through `Adopt`. tmux
  hooks become hints, not evidence.
- **Attach** returns `tmux attach -t =session \; select-pane -t %N` for tmux.
  For headless, it returns the backend's own resume where one exists (for
  example a Codex TUI attached to the same App Server thread; this has to be
  verified per backend before it is promised), else none. The dashboard's
  transcript view stays the universal observe path.
- **Restart policy** stays where it is: `restart_peer` is an explicit
  action. A container never restarts an agent by itself; an unexpected exit
  marks the peer offline and records why.

### What moves

- `TmuxController` becomes the tmux `Container`. `PaneOwnership` becomes
  container-neutral ownership records keyed by peer id, carrying the handle.
- `paneSafe`, `findExpectedAgentPID` and the pane-death strikes in reconcile
  collapse into one check: is the handle's pid with its start time alive.
- The tmux lifecycle hooks keep only what they alone know (renames for
  circle locators). Pane death and session close stop driving offline
  state, because the process watch is authoritative.
- Circle defaults stay tmux-derived for peers started in tmux; headless
  launches take an explicit circle, as non-tmux launches already do.

### What we lose without tmux (for headless peers only)

Live human attach to a TUI and its scrollback, interactive permission and
trust prompts (headless backends need an explicit approval policy instead),
and survival across a daemon restart for backends whose headless process is
a daemon child (ACP today). That last one is the hard edge: a headless plug
must either run the agent detached and re-attach to its protocol (the Codex
App Server and `opencode serve` are servers; ACP over stdio is not), or
accept that a daemon restart ends those agents. That needs to be stated per
backend.

### Which tmux pains go away

- **Spurious `session_closed`:** gone as a source of offline state. The
  process exit watch decides; a session event is at most a hint (#376's
  evidence gate becomes unnecessary).
- **`kill_peer` leaving windows or shells:** the container kills the process
  group and removes its place (tmux: the pane, and the window when it was
  the last pane the container created). The leftover-shell clean-up in
  `peer_restart` becomes part of `Kill`.
- **Trust prompts:** gone for headless launches (non-interactive with an
  approval policy). They remain for TUIs in tmux, where a human is present
  by design.
- **Pane-proof inference** (#532, #544, #552, #408): replaced by the handle's
  pid and start time.

## 4. Order of work

1. **Socket and exit watch, tmux only, no behaviour change for users.**
   Extract `Container` from `TmuxController`, store `{pid, started_at,
   pgid}` in ownership records at spawn, watch the agent pid (pidfd or
   kqueue), and re-adopt the watches on daemon start. Use the exit watch for
   offline state, and keep the tmux hooks only as hints. Kill uses the handle
   check. This removes the spurious-offline and proof-inference class for
   spawned peers.
2. **A headless plug for ACP peers**, moving `ACPManager`'s process ownership
   behind the socket (it already is a headless container in all but name).
3. **Headless for the backends with a server mode** (Codex App Server
   threads, OpenCode), with per-backend attach and restart-survival
   statements.
4. **Optional:** `repowire attach <peer>` as the one human entry point over
   either plug.

Peers a human starts by hand (not spawned) are outside the container: they
register through hooks as today, and kill or restart keeps refusing them
unless their pane metadata proves the agent, as now.

## Open questions

- On Linux without pidfd (kernel older than 5.3), fall back to polling the
  pid with its start time on request (lazy, never on a timer)?
- For the headless Codex plug, can a human TUI attach to a running App
  Server thread without becoming a second writer? This needs verifying on
  codex-cli 0.171.
- Should a headless peer have a default circle (its spawner's), or always
  require one?
