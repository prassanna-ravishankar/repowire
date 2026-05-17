# ACP probe matrix (repowire #164)

Per-adapter capability probes for the 12 ACP capabilities identified in §8 of
`acp-mapping-delta-claude.md`. Each adapter has its own module under `probes/`
encoding the same 12 probe rows; results combine into a single matrix at
`experiments/acp-probe-matrix.md`.

## Layout

- `client.py` — shared `RecordingClient` (implements the ACP `Client` protocol;
  records every callback the agent makes) + helpers.
- `probes/gemini.py` — `gemini --experimental-acp` (native flag).
- `probes/copilot.py` — `gh copilot --acp` (native, public preview).
- `probes/claude_agent.py` — `claude-agent-acp` Zed adapter.
- `probes/codex.py` — `codex-acp` Zed adapter.
- `probes/pi.py` — `pi-acp` adapter.

## Running

```bash
# install the optional deps
uv sync --extra acp-probe

# run one adapter's full matrix
python experiments/acp-probe/probes/gemini.py
```

Each probe is independent — a single failure doesn't abort the rest of the row.

## Capability rows

| # | Name | What it tests |
|---|---|---|
| C1 | initialize handshake | `agent_capabilities` populated |
| C2 | session/new + cwd | sessionId returned, cwd respected |
| C3 | streaming chunks | ≥2 `agent_message_chunk` frames per turn |
| C4 | tool_call lifecycle | `ToolCallStart` → `ToolCallProgress` |
| C5 | request_permission | client gets a permission round-trip |
| C6 | session/cancel | `StopReason=cancelled` (not error) |
| C7 | session/load replay | prior chunks + tool calls replayed |
| C8 | session/close | capability-gated clean close |
| C9 | fs/read/write | scoped reads + writes via client callbacks |
| C10 | terminal/* | (deferred — needs terminal handlers on `RecordingClient`) |
| C11 | plan updates | `AgentPlanUpdate` notifications |
| C12 | content blocks | image + audio in `prompt_capabilities` |

A row's outcome is one of `pass` / `fail` / `partial` / `n/a`. `n/a` means the
adapter does not advertise the capability (legitimate); `partial` means the
behaviour exists but is best-effort (e.g. C3 streaming may coalesce).
