# Hook Payloads

Hook payloads vary by agent runtime. Repowire normalizes them before handler code updates peer state or extracts responses. Current Codex releases retain only the Stop payload's `session_id` for pending-ask reminders; the other Codex entries describe the full fallback used when App Server listening is unavailable.

## Normalized concepts

| Concept | Claude Code | Codex | Gemini / Antigravity |
| --- | --- | --- | --- |
| Prompt event | `UserPromptSubmit` | `UserPromptSubmit` | `BeforeAgent` |
| Stop event | `Stop` | `Stop` | `AfterAgent` |
| Response field | transcript JSONL | `last_assistant_message` | `prompt_response` |
| Hook output | empty | empty | `{"decision": "allow"}` |

## Runtime model capture

Repowire stores the observed runtime model on `peer.model` when a backend
explicitly reports it. This is runtime state, not spawn intent: Repowire does
not parse `--model` out of command strings or spawn profiles.

| Backend | Capture path | Gotcha |
| --- | --- | --- |
| Claude Code | `SessionStart` hook `model` field. | Claude Code exposes model on `SessionStart`; later hook events may not refresh it. |
| Codex | App Server thread metadata; fallback hooks expose a `model` field. | The native thread summary may not expose a concrete model, so it can remain unknown. |
| Gemini | Best-effort explicit hook `model` field, if present. | Current Repowire Gemini path has no confirmed model field. |
| Antigravity | Same best-effort extraction as Gemini once hooks fire. | `agy` hook firing is still pending upstream, so daemon-spawned CLI-fallback peers usually have no model. |
| OpenCode | Plugin `message.updated` user-message model; `modelID` becomes `peer.model`, provider/model details stay in metadata. | Model may be unknown until OpenCode emits its first user-message model info. |
| Pi | None in v1. | The current extension path has no clear model accessor/event. |
| MCP HTTP | Caller-supplied `model` on register/connect/update. | External transport is authoritative; the daemon does no inference. |

## SessionEnd

`SessionEnd` fires once at a true session boundary and carries a `reason`
(Claude Code: `prompt_input_exit` for `/exit`, `clear` for `/clear`, `logout`,
`other`). The handler always writes the handoff summary, then — for any
reason other than `clear` — resolves the pane's peer_id and posts a terminal
offline (`reason=session_end`), deregistering the peer immediately. `clear` is
skipped because a `SessionStart` with `source=clear` rebinds the same pane
milliseconds later. It does not fire on SIGKILL; the ws-hook's agent-pid
watcher covers that case.

On current Codex releases, App Server's `thread/closed` event ends the live peer.
The older hooks fallback has no SessionEnd event, so quit deregistration relies
on the agent-pid watcher.

## Default delivery path

The hooks + MCP transport uses hooks for lifecycle and Stop-hook reminders, MCP
for outbound commands, and tmux pane injection for live inbound delivery. Codex
uses App Server thread steering instead when available.

## Related

- [Operate: transports](../operate/transports.md)
- [Troubleshooting: hooks not firing](../troubleshooting/hooks.md)
- [Connect Claude Code](../use/features/connect-claude-code.md)
