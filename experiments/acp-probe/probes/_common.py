"""Shared probe definitions reused by every adapter module.

The 12 probes from acp-mapping-delta-claude.md §8 are identical across adapters
in structure — only ``COMMAND`` and a few timeouts differ. Adapter modules set
``COMMAND``, then call ``run_all(COMMAND)`` to execute the full battery.
"""

from __future__ import annotations

import asyncio
import sys
import tempfile
from collections.abc import Awaitable, Callable
from pathlib import Path

import acp
from client import ProbeResult, open_agent

TIMEOUT = 30.0
LONG_TIMEOUT = 90.0


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
    assert resp.agent_capabilities is not None, "agent did not return agent_capabilities"
    return resp.agent_capabilities


async def c1_initialize(command: tuple[str, ...]) -> ProbeResult:
    try:
        async with open_agent(*command) as (conn, _rec):
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


async def c2_new_session_cwd(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                resp = await asyncio.wait_for(
                    _new_session(conn, str(Path(tmp).resolve())), timeout=TIMEOUT
                )
                ok = bool(resp.session_id)
                return ProbeResult("C2", "pass" if ok else "fail", f"session_id={resp.session_id}")
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C2", "fail", f"{type(e).__name__}: {e}")


async def c3_streaming_chunks(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, rec):
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


async def c4_tool_call_lifecycle(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "hello.txt"
            target.write_text("repowire-acp-probe\n")
            async with open_agent(*command, cwd=tmp) as (conn, rec):
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


async def c5_request_permission(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, rec):
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


async def c6_cancel_midturn(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                prompt_task = asyncio.create_task(
                    _prompt(
                        conn, sess.session_id,
                        "Write a 2000-word essay about the history of unix pipes.",
                    )
                )
                await asyncio.sleep(2.0)
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


async def c7_load_session_replay(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, _rec):
                init = await _initialize(conn)
                if not _caps(init).load_session:
                    return ProbeResult("C7", "n/a", "agent does not advertise load_session")
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id, "Say 'session-marker-zoltar' and nothing else."
                    ),
                    timeout=60.0,
                )
                session_id = sess.session_id
            async with open_agent(*command, cwd=tmp) as (conn, rec):
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
                    return ProbeResult(
                        "C7", "fail", f"load_session raised {type(e).__name__}: {e}"
                    )
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


async def c8_close_session(command: tuple[str, ...]) -> ProbeResult:
    try:
        async with open_agent(*command) as (conn, _rec):
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


async def c9_fs_read_write(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "scratch.txt"
            target.write_text("before\n")
            async with open_agent(*command, cwd=tmp) as (conn, _rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        f"Read {target}, then overwrite it with the single word 'after'.",
                    ),
                    timeout=LONG_TIMEOUT,
                )
                mutated = target.read_text().strip() == "after"
                return ProbeResult(
                    "C9",
                    "pass" if mutated else "partial",
                    f"file_after={target.read_text()!r}",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C9", "fail", f"{type(e).__name__}: {e}")


async def c10_terminal(_command: tuple[str, ...]) -> ProbeResult:
    return ProbeResult(
        "C10",
        "n/a",
        "terminal probe requires RecordingClient terminal handlers; deferred",
    )


async def c11_plan_updates(command: tuple[str, ...]) -> ProbeResult:
    try:
        with tempfile.TemporaryDirectory() as tmp:
            async with open_agent(*command, cwd=tmp) as (conn, rec):
                await _initialize(conn)
                sess = await _new_session(conn, str(Path(tmp).resolve()))
                await asyncio.wait_for(
                    _prompt(
                        conn, sess.session_id,
                        "I want to refactor a python file with three steps: read it, "
                        "propose changes, write it back. Make a plan before doing anything.",
                    ),
                    timeout=LONG_TIMEOUT,
                )
                plans = [u for u in rec.updates if type(u.update).__name__ == "AgentPlanUpdate"]
                return ProbeResult(
                    "C11",
                    "pass" if plans else "fail",
                    f"{len(plans)} plan updates",
                )
    except Exception as e:  # noqa: BLE001
        return ProbeResult("C11", "fail", f"{type(e).__name__}: {e}")


async def c12_content_blocks(command: tuple[str, ...]) -> ProbeResult:
    try:
        async with open_agent(*command) as (conn, _rec):
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


Probe = Callable[[tuple[str, ...]], Awaitable[ProbeResult]]
ALL_PROBES: list[Probe] = [
    c1_initialize, c2_new_session_cwd, c3_streaming_chunks, c4_tool_call_lifecycle,
    c5_request_permission, c6_cancel_midturn, c7_load_session_replay, c8_close_session,
    c9_fs_read_write, c10_terminal, c11_plan_updates, c12_content_blocks,
]


async def run_all(adapter: str, command: tuple[str, ...]) -> list[ProbeResult]:
    """Run every probe against the adapter, printing progress to stderr."""
    results: list[ProbeResult] = []
    for probe in ALL_PROBES:
        name = getattr(probe, "__name__", repr(probe))
        print(f"[{adapter}] running {name}...", file=sys.stderr)
        r = await probe(command)
        print(f"  → {r.capability}: {r.status} — {r.detail}", file=sys.stderr)
        results.append(r)
    return results


def emit_results(adapter: str, results: list[ProbeResult]) -> None:
    print(f"# {adapter} probe results")
    for r in results:
        print(f"- {r.capability}: **{r.status}** — {r.detail}")
