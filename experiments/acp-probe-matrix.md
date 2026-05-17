# ACP probe matrix — repowire #164

Per-adapter ACP capability probe results, run 2026-05-17 via
`experiments/acp-probe/`. Probe design lives in §8 of
`acp-mapping-delta-claude.md`; this file is the executed artifact.

## Adapters probed

| Adapter | Binary | Version | Type |
|---|---|---|---|
| gemini | `gemini --experimental-acp` | nvm/node v23.9.0 | native CLI flag |
| copilot | `copilot --acp` | `@github/copilot@1.0.48` | native CLI flag |
| claude-agent-acp | `claude-agent-acp` | `@agentclientprotocol/claude-agent-acp` (was `@zed-industries/claude-code-acp@0.16.2`) | Zed translation adapter |
| codex-acp | `codex-acp` | `@zed-industries/codex-acp@0.14.0` | Zed translation adapter |
| pi-acp | `pi-acp` | `pi-acp@0.0.27` | community adapter |

All five installed via `npm i -g`. No additional configuration; each adapter
used its existing local auth (OAuth or API key) at first invocation.

## Matrix

`pass` = capability behaves as expected. `partial` = behaviour exists but
degraded (e.g. coalesced streaming, agent auto-allowed instead of asking
permission). `fail` = expected behaviour absent. `n/a` = capability not
advertised by the adapter (this is the legitimate "I don't support that"
outcome, not a regression).

|     | gemini   | copilot  | claude-agent-acp | codex-acp | pi-acp   |
|-----|----------|----------|------------------|-----------|----------|
| C1  | pass     | pass     | pass             | pass      | pass     |
| C2  | pass     | pass     | pass             | pass      | pass     |
| C3  | partial† | pass (9) | pass (2)         | pass (9)  | pass (11)|
| C4  | pass     | pass     | pass             | pass      | pass     |
| C5  | pass     | pass     | partial‡         | pass      | partial‡ |
| C6  | pass     | partial§ | pass             | pass      | pass     |
| C7  | fail¶    | pass     | pass             | pass      | pass     |
| C8  | n/a      | n/a      | pass             | pass      | n/a      |
| C9  | pass     | pass     | pass             | pass      | pass     |
| C10 | n/a‖     | n/a‖     | n/a‖             | n/a‖      | n/a‖     |
| C11 | fail     | pass     | fail             | fail      | fail     |
| C12 | pass⊕    | pass     | pass             | pass      | pass     |

**Totals (out of 12, treating n/a as neutral):**
- gemini: 7 pass · 1 partial · 2 fail · 2 n/a
- copilot: **9 pass** · 1 partial · 0 fail · 2 n/a
- claude-agent-acp: 9 pass · 1 partial · 1 fail · 1 n/a
- codex-acp: **10 pass** · 0 partial · 1 fail · 1 n/a
- pi-acp: 8 pass · 1 partial · 1 fail · 2 n/a

### Footnotes

† **gemini C3 partial — non-deterministic.** Gemini coalesces short responses
into a single `agent_message_chunk` even when prompted for multi-line output.
Probe alternates between `pass` (≥2 chunks) and `partial` (1 chunk) across
runs. This is a real finding for §4.3 of `claudecodeui-adoption.md`:
token-level streaming is best-effort on gemini, not guaranteed.

‡ **C5 partial = agent auto-allowed.** claude-agent-acp and pi-acp completed
the file-creation prompt without invoking `session/request_permission`.
The capability path exists (others adapters invoke it on the same prompt);
these adapters chose not to gate. Whether this is a security defaulting
issue depends on configuration — not investigated.

§ **copilot C6 partial — `end_turn` instead of `cancelled`.** Copilot
finished the unix-pipes essay turn before the `session/cancel` signal
landed. The cancel-mid-turn race window is real but small. Probe
needs a longer-running prompt to demonstrate the cancellation path
on copilot; deferred.

¶ **gemini C7 fail.** Gemini advertises `load_session=True` in
`agent_capabilities`. A subsequent `session/load` against the prior session
id returned cleanly but produced 0 replayed chunks — no `UserMessageChunk`
or `AgentMessageChunk` notifications arrived during replay. Either the
adapter's load is a no-op (advertises but doesn't replay) or the SDK isn't
surfacing them — to be re-verified before relying on §4.1 transcript-hybrid
behaviour for the gemini path.

‖ **All C10 n/a — terminal probe deferred.** The terminal capability path
exists in the SDK but requires extending `RecordingClient` with
`create_terminal`/`terminal_output`/`release_terminal` handlers. Tracked as
follow-up; not blocking.

⊕ **gemini C12 reports `image=True, audio=True`.** Every other adapter
reports `image=True, audio=False`. So gemini is the only one that advertises
audio content blocks in prompts. Useful for `claudecodeui-adoption` if voice
input becomes a real surface.

## Key findings

1. **The Zed translation adapters outperform the native flags.** Codex-acp
   passes 10/12 — more than gemini's native `--experimental-acp` (7/12).
   `claude-agent-acp` matches it modulo the auto-allow on permission. This
   inverts the pre-probe assumption in §8 of `acp-mapping-delta-claude.md`
   that translation layers would be the risky ones. The Zed adapters
   appear to be more rigorously ACP-conformant than the vendors' own
   first-party CLI flags.

2. **C7 (`session/load`) is real for every adapter except gemini.** This
   means §4.1 (transcript-hybrid) of `claudecodeui-adoption.md` can be
   `session/load`-first for claude-agent / codex / copilot / pi peers,
   falling back to the JSONL parser only for gemini and offline peers.

3. **C8 (`session/close`) is advertised by only claude-agent-acp and
   codex-acp.** Gemini, copilot, and pi-acp do not. For the `kill_peer`
   clean path (§1 mapping table row), the broker must capability-gate:
   advertised → `session/close` + transport drop + tmux kill; not
   advertised → transport drop + tmux kill (current behaviour).

4. **C11 (`plan` updates) is only emitted by copilot.** Every other adapter
   failed C11 even when explicitly prompted to plan. Two interpretations:
   (a) the adapters don't translate model-side plan output into ACP `plan`
   frames, or (b) the models themselves aren't producing plans for our
   prompt. Either way, open question #7 in
   `acp-mapping-delta-claude.md` (plan-as-event) should not block on this
   capability being universal — promote it as a copilot-only feature for now.

5. **Streaming chunk counts vary 10x across adapters.** pi-acp emits 11
   chunks, codex-acp and copilot emit 9, claude-agent-acp emits 2, gemini
   emits 1-2 non-deterministically. The user-perceived "typing" experience
   from §4.3 will differ visibly per backend.

6. **`session/request_permission` UX defaults differ.** Copilot, codex-acp,
   and gemini routinely route file-write tools through permission;
   claude-agent-acp and pi-acp auto-allow. Operationally important: the
   broker's permission-UX work (open question #6 in
   `acp-mapping-delta-claude.md`) is per-adapter, not universal.

## What didn't get probed

- **C10 terminals.** Needs `RecordingClient` extension. Follow-up PR.
- **C5 in detail.** Probe records *count* of permission round-trips, not
  the option-set offered or whether auto-deny works. Sufficient for
  baseline; insufficient for permission-UX design.
- **C12 audio block submission.** Probe only inspects advertised caps,
  doesn't actually try to send an audio block.
- **Multi-turn cancellation.** C6 cancels mid-first-turn only. A
  multi-step plan being cancelled at step 3 may behave differently.
- **Replay fidelity beyond the marker.** C7 checks the marker survives
  the round-trip but doesn't verify tool-call results, plan entries, or
  permission-request transcripts re-appear.

## Migration implications for repowire #163

Reading the matrix against `acp-mapping-delta-claude.md` §5 (the 5-step
migration plan):

- **Step 1 (ACP client inside broker, behind a flag)** — green light.
  Every adapter passes C1, C2. The broker can open an ACP session
  against any of the five today.
- **Step 2 (`chat_turn_delta` over ACP)** — green for copilot, codex-acp,
  pi-acp (real chunking); usable but degraded for claude-agent-acp
  (2 chunks); not a streaming win on gemini until they improve their flag.
- **Step 3 (filesystem + terminal via ACP)** — fs is green everywhere;
  terminal needs the C10 deferred work first.
- **Step 4 (ACP-first default)** — the per-adapter quality variance means
  this is "ACP-first for backends where it adds value." Gemini may stay
  on the MCP path until its adapter matures; pi-acp is surprisingly
  strong despite the §8 prediction.
- **Step 5 (transcript hybrid + backend switcher)** — `session/load`
  fallback to JSONL is needed for gemini only. For the other four,
  `session/load` is the primary path.

## Reproducing

```bash
# install adapters (npm-global)
npm i -g @github/copilot @agentclientprotocol/claude-agent-acp \
        @zed-industries/codex-acp pi-acp
# gemini is assumed pre-installed (it ships under another flow)

# install probe deps
uv sync --extra acp-probe

# run one adapter
python experiments/acp-probe/probes/copilot.py > results/copilot.md

# raw run logs live alongside under experiments/acp-probe/results/
```
