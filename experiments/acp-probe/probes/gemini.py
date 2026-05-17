"""Per-capability probes for `gemini --experimental-acp` (native adapter).

Encodes the 12 capability rows from acp-mapping-delta-claude.md §8. Native flag
is expected to pass C1–C10; C11/C12 unknown going in. Each probe returns a
``ProbeResult`` and is independent — failures don't abort the matrix.

Run:
    python -m probes.gemini
"""

from __future__ import annotations

import asyncio
import sys
import tempfile
from pathlib import Path

# Make sibling client.py importable whether run as module or script.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import acp  # noqa: E402
from client import ProbeResult, open_agent  # noqa: E402

ADAPTER = "gemini"
COMMAND = ("gemini", "--experimental-acp")
TIMEOUT = 30.0


async def _initialize(conn: acp.client.connection.ClientSideConnection) -> acp.InitializeResponse:
    return await conn.initialize(protocol_version=acp.PROTOCOL_VERSION)


async def _new_session(conn: acp.client.connection.ClientSideConnection, cwd: str):
    return await conn.new_session(cwd=cwd, mcp_servers=[])


async def _prompt(conn: acp.client.connection.ClientSideConnection, session_id: str, text: str):
    return await conn.prompt(
        prompt=[acp.text_block(text)],
        session_id=session_id,
    )


def _caps(resp: acp.InitializeResponse):
    """Unwrap agent_capabilities, asserting it's present. Adapters MUST send caps."""
    assert resp.agent_capabilities is not None, "agent did not return agent_capabilities"
    return resp.agent_capabilities


async def c1_initialize() -> ProbeResult:
    """C1: initialize handshake — agentCapabilities present."""
    try:
        async with open_agent(*COMMAND) as (conn, _rec):
            resp = await asyncio.wait_for(_initialize(conn), timeout=TIMEOUT)
            ok = resp.agent_capabilities is not None
            return ProbeResult(
                "C1",
                "pass" if ok else "partial",
                f"protocol_version={resp.protocol_version}; "
                f"load_session={_caps(resp).load_session}",
            )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C1", "fail", f"{type(e).__name__}: {e}")


async def c2_new_session_cwd() -> ProbeResult:
    """C2: session/new with absolute cwd returns a sessionId."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*COMMAND, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                resp = await asyncio.wait_for(
                    _new_session(conn, str(Path(tmp).resolve())), timeout=TIMEOUT
                )
                ok = bool(resp.session_id)
                return ProbeResult("C2", "pass" if ok else "fail", f"session_id={resp.session_id}")
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C2", "fail", f"{type(e).__name__}: {e}")


async def c3_streaming_chunks() -> ProbeResult:
    """C3: session/prompt produces ≥2 agent_message_chunk notifications.

    Note: chunking is non-deterministic. Gemini sometimes coalesces a short
    response into a single frame even when prompted for multi-line output.
    Result oscillates between "pass" (≥2 chunks) and "partial" (1 chunk).
    A single observation of "partial" should be treated as evidence that
    streaming is best-effort, not guaranteed.
    """
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*COMMAND, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                prompt_resp = await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id, "Count from one to five, one number per line."
                    ),
                    timeout=60.0,
                )
                chunks = [u for u in rec.updates if type(u.update).__name__ == "AgentMessageChunk"]
                status = (
                    "pass" if len(chunks) >= 2
                    else "partial" if len(chunks) == 1
                    else "fail"
                )
                return ProbeResult(
                    "C3",
                    status,
                    f"{len(chunks)} agent_message_chunk frames, "
                    f"stop_reason={prompt_resp.stop_reason}",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C3", "fail", f"{type(e).__name__}: {e}")


async def c4_tool_call_lifecycle() -> ProbeResult:
    """C4: tool_call (pending) → tool_call_update (completed) for a forced tool use."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "hello.txt"
            target.write_text("repowire-acp-probe\n")
            async with open_agent(*COMMAND, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        f"Read the file {target} and tell me its contents.",
                    ),
                    timeout=60.0,
                )
                starts = [u for u in rec.updates if type(u.update).__name__ == "ToolCallStart"]
                progresses = [
                    u for u in rec.updates if type(u.update).__name__ == "ToolCallProgress"
                ]
                if starts and progresses:
                    return ProbeResult(
                        "C4", "pass", f"{len(starts)} start, {len(progresses)} progress"
                    )
                if starts:
                    return ProbeResult("C4", "partial", f"{len(starts)} start, no progress")
                return ProbeResult("C4", "fail", "no tool_call frames observed")
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C4", "fail", f"{type(e).__name__}: {e}")


async def c5_request_permission() -> ProbeResult:
    """C5: request_permission round-trip on a write-sensitive tool."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*COMMAND, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        f"Create a file at {tmp}/new.txt containing the word 'hello'.",
                    ),
                    timeout=60.0,
                )
                n = len(rec.permission_requests)
                return ProbeResult(
                    "C5",
                    "pass" if n >= 1 else "partial",
                    f"{n} request_permission round-trips "
                    f"({'agent auto-allowed' if n == 0 else 'OK'})",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C5", "fail", f"{type(e).__name__}: {e}")


async def c6_cancel_midturn() -> ProbeResult:
    """C6: session/cancel mid-turn → StopReason=cancelled (not error)."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*COMMAND, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                prompt_task = asyncio.create_task(
                    _prompt(
                        conn, sess.session_id,
                        "Write a 2000-word essay about the history of unix pipes.",
                    )
                )
                await asyncio.sleep(2.0)  # let the turn get going
                await conn.cancel(sess.session_id)
                resp = await asyncio.wait_for(prompt_task, timeout=30.0)
                stop = str(resp.stop_reason).lower()
                return ProbeResult(
                    "C6",
                    "pass" if "cancel" in stop else "partial",
                    f"stop_reason={resp.stop_reason}",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C6", "fail", f"{type(e).__name__}: {e}")


async def c7_load_session_replay() -> ProbeResult:
    """C7: session/load replays prior chunks + tool calls."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            # Turn 1: build history
            async with open_agent(*COMMAND, cwd=tmp) as (conn, _rec):
                init = await _initialize(conn)
                if not _caps(init).load_session:
                    return ProbeResult("C7", "n/a", "agent does not advertise load_session")
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(conn, sess.session_id, "Say 'session-marker-zoltar' and nothing else."),
                    timeout=60.0,
                )
                session_id = sess.session_id
            # Turn 2: fresh process, load
            async with open_agent(*COMMAND, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                try:
                    await asyncio.wait_for(
                        conn.load_session(
                            cwd=str(Path(tmp).resolve()),
                            session_id=session_id,
                            mcp_servers=[],
                        ),
                        timeout=30.0,
                    )
                except Exception as e:  # noqa: BLE001
                    return ProbeResult("C7", "fail", f"load_session raised {type(e).__name__}: {e}")
                replayed = [
                    u for u in rec.updates
                    if type(u.update).__name__ in {"UserMessageChunk", "AgentMessageChunk"}
                ]
                ok = any(
                    "zoltar" in str(getattr(u.update, "content", "")).lower() for u in replayed
                )
                return ProbeResult(
                    "C7",
                    "pass" if ok else ("partial" if replayed else "fail"),
                    f"{len(replayed)} replayed chunks, marker_found={ok}",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C7", "fail", f"{type(e).__name__}: {e}")


async def c8_close_session() -> ProbeResult:
    """C8: session/close — capability-gated."""
    try:
        async with open_agent(*COMMAND) as (conn, _rec):
            init = await _initialize(conn)
            close_cap = getattr(_caps(init).session_capabilities, "close", None)
            if not close_cap:
                return ProbeResult(
                    "C8", "n/a", "agent does not advertise session_capabilities.close"
                )
            with tempfile.TemporaryDirectory() as tmp:
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await conn.close_session(sess.session_id)
                return ProbeResult("C8", "pass", "close_session returned cleanly")
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C8", "fail", f"{type(e).__name__}: {e}")


async def c9_fs_read_write() -> ProbeResult:
    """C9: fs/read_text_file and fs/write_text_file scoped to session cwd.

    Verified indirectly — RecordingClient implements both, so if the agent
    invokes them during a prompt, we observe (and the file mutates).
    """
    try:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "scratch.txt"
            target.write_text("before\n")
            async with open_agent(*COMMAND, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        f"Read {target}, then overwrite it with the single word 'after'.",
                    ),
                    timeout=90.0,
                )
                mutated = target.read_text().strip() == "after"
                return ProbeResult(
                    "C9",
                    "pass" if mutated else "partial",
                    f"file_after={target.read_text()!r}",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C9", "fail", f"{type(e).__name__}: {e}")


async def c10_terminal() -> ProbeResult:
    """C10: terminal/* lifecycle — depends on adapter wiring."""
    return ProbeResult(
        "C10",
        "n/a",
        "terminal probe requires RecordingClient terminal handlers; deferred to follow-up PR",
    )


async def c11_plan_updates() -> ProbeResult:
    """C11: plan notifications arrive with priority+status entries."""
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*COMMAND, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        "I want to refactor a python file with three steps: read it, "
                        "propose changes, write it back. Make a plan before doing anything.",
                    ),
                    timeout=90.0,
                )
                plans = [u for u in rec.updates if type(u.update).__name__ == "AgentPlanUpdate"]
                return ProbeResult(
                    "C11",
                    "pass" if plans else "fail",
                    f"{len(plans)} plan updates",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C11", "fail", f"{type(e).__name__}: {e}")


async def c12_content_blocks() -> ProbeResult:
    """C12: prompt content-block coverage (text + image). Negotiated via prompt_capabilities."""
    try:
        async with open_agent(*COMMAND) as (conn, _rec):
            init = await _initialize(conn)
            caps = _caps(init).prompt_capabilities
            supports_image = bool(getattr(caps, "image", False))
            supports_audio = bool(getattr(caps, "audio", False))
            return ProbeResult(
                "C12",
                "pass" if supports_image else "partial",
                f"image={supports_image}, audio={supports_audio}",
            )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C12", "fail", f"{type(e).__name__}: {e}")


PROBES = [
    c1_initialize, c2_new_session_cwd, c3_streaming_chunks, c4_tool_call_lifecycle,
    c5_request_permission, c6_cancel_midturn, c7_load_session_replay, c8_close_session,
    c9_fs_read_write, c10_terminal, c11_plan_updates, c12_content_blocks,
]


async def run_all() -> list[ProbeResult]:
    results: list[ProbeResult] = []
    for probe in PROBES:
        print(f"[gemini] running {probe.__name__}...", file=sys.stderr)
        r = await probe()
        print(f"  → {r.capability}: {r.status} — {r.detail}", file=sys.stderr)
        results.append(r)
    return results


if __name__ == "__main__":
    results = asyncio.run(run_all())
    print()
    print(f"# {ADAPTER} probe results")
    for r in results:
        print(f"- {r.capability}: **{r.status}** — {r.detail}")
