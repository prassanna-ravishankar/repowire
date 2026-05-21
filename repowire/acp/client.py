"""Broker-side ACP client.

One ``AcpClient`` instance owns one ACP subprocess and one persistent session
(per peer). The session is created lazily on first ``prompt`` and reused across
asks so context is preserved within the peer.

Streaming ``session/update`` notifications (``agent_message_chunk``,
``tool_call``, ``plan``…) are recorded into the ``AcpPromptResult`` returned
from ``prompt``. Phase-3 surfaces the accumulated assistant text as the ack
reply; later phases will translate the same updates into ``chat_turn_delta``
mesh-bus events.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any

from repowire.acp.models import AcpPeerConfig
from repowire.acp.permissions import ApprovalBroker, PermissionDecision
from repowire.acp.transport import spawn_acp_subprocess

if TYPE_CHECKING:
    from acp.client.connection import ClientSideConnection

logger = logging.getLogger(__name__)


class AcpClientError(RuntimeError):
    """Wrapper for ACP failures (initialize, new_session, prompt). Always retriable."""


@dataclass
class AcpPromptResult:
    """Result of one ``session/prompt`` round-trip.

    ``text`` is the concatenated ``agent_message_chunk`` body — what the broker
    surfaces as the ack reply to the original asker. ``stop_reason`` is the
    raw ACP termination reason; ``end_turn`` is the success case.
    """

    text: str
    stop_reason: str
    updates: list[Any] = field(default_factory=list)


_ACP_INSTALL_HINT = (
    "Install the optional ACP runtime: `uv tool upgrade --reinstall "
    "--with agent-client-protocol repowire` (or `pip install "
    "'repowire[acp]'`). Then restart the daemon. The ACP broker only "
    "starts subprocesses when experiments.acp_broker_client is true."
)


def _make_recorder(
    *,
    peer_id: str | None = None,
    approval_broker: ApprovalBroker | None = None,
) -> Any:
    """Build a fresh ``acp.Client`` instance that records updates for one peer.

    Defined as a factory rather than a top-level class so the ``acp`` import
    stays lazy — the broker only imports the SDK when the ACP path is wired up.
    Raises ``AcpClientError`` with an install hint when the optional ACP
    runtime is missing.
    """
    try:
        import acp
        from acp.schema import AllowedOutcome, DeniedOutcome
    except ModuleNotFoundError as e:
        raise AcpClientError(
            f"agent-client-protocol SDK not installed: {e}. {_ACP_INSTALL_HINT}"
        ) from e

    class _BrokerRecorder(acp.Client):
        """Records ``session/update`` notifications for the broker.

        Permission requests are routed through the daemon-side approval broker.
        When no broker is configured, requests fail closed.
        """

        def __init__(self) -> None:
            self.updates: list[Any] = []

        async def session_update(self, session_id: str, update: Any, **_: Any) -> None:
            del session_id  # logged at debug level below
            self.updates.append(update)
            logger.debug("ACP session/update kind=%s", type(update).__name__)

        async def request_permission(
            self,
            options: list[Any],
            session_id: str,
            tool_call: Any,
            **_: Any,
        ) -> Any:
            if approval_broker is None or peer_id is None:
                return acp.RequestPermissionResponse(outcome=DeniedOutcome(outcome="cancelled"))
            decision = await approval_broker.request_permission(
                peer_id=peer_id,
                session_id=session_id,
                tool_call=tool_call,
                options=options,
            )
            if decision.outcome != "allowed":
                return acp.RequestPermissionResponse(
                    outcome=DeniedOutcome(outcome="cancelled"),
                )
            chosen = _select_allowed_option(options, decision)
            if chosen is None:
                return acp.RequestPermissionResponse(outcome=DeniedOutcome(outcome="cancelled"))
            return acp.RequestPermissionResponse(
                outcome=AllowedOutcome(outcome="selected", option_id=chosen.option_id),
            )

        async def read_text_file(
            self,
            path: str,
            session_id: str,
            limit: int | None = None,
            line: int | None = None,
            **_: Any,
        ) -> Any:
            del session_id, limit, line
            return acp.ReadTextFileResponse(content=Path(path).read_text())

        async def write_text_file(
            self,
            content: str,
            path: str,
            session_id: str,
            **_: Any,
        ) -> Any:
            del session_id
            Path(path).write_text(content)
            return acp.WriteTextFileResponse()

    return _BrokerRecorder()


class AcpClient:
    """One ACP subprocess + one persistent session, per peer.

    Use as an async context manager (``async with AcpClient(cfg) as c: await c.prompt(...)``)
    or via ``AcpClientManager`` which handles the lifetime.
    """

    def __init__(
        self,
        config: AcpPeerConfig,
        *,
        fallback_cwd: str | None = None,
        peer_id: str | None = None,
        approval_broker: ApprovalBroker | None = None,
    ) -> None:
        self._config = config
        self._fallback_cwd = fallback_cwd
        self._recorder = _make_recorder(peer_id=peer_id, approval_broker=approval_broker)
        self._connection: ClientSideConnection | None = None
        self._session_id: str | None = None
        self._exit_stack: Any = None
        self._lock = asyncio.Lock()
        self._closed = False
        self._crashed = False

    @property
    def session_id(self) -> str | None:
        return self._session_id

    async def __aenter__(self) -> AcpClient:
        await self._ensure_started()
        return self

    async def __aexit__(self, *exc_info: Any) -> None:
        del exc_info
        await self.close()

    async def _ensure_started(self) -> None:
        if self._connection is not None:
            return
        import contextlib

        import acp

        self._exit_stack = contextlib.AsyncExitStack()
        await self._exit_stack.__aenter__()
        cwd = self._config.cwd or self._fallback_cwd
        try:
            sub = await self._exit_stack.enter_async_context(
                spawn_acp_subprocess(
                    self._recorder,
                    self._config.command,
                    *self._config.args,
                    cwd=cwd,
                    env=self._config.env,
                )
            )
            self._connection = sub.connection
            await self._connection.initialize(protocol_version=acp.PROTOCOL_VERSION)
        except Exception as e:
            await self._exit_stack.__aexit__(type(e), e, e.__traceback__)
            self._exit_stack = None
            self._connection = None
            raise AcpClientError(f"failed to start ACP subprocess: {e}") from e

    async def _ensure_session(self) -> str:
        if self._session_id is not None:
            return self._session_id
        assert self._connection is not None
        cwd = self._config.cwd or self._fallback_cwd or str(Path.cwd())
        try:
            resp = await self._connection.new_session(cwd=str(Path(cwd).resolve()), mcp_servers=[])
        except Exception as e:
            raise AcpClientError(f"new_session failed: {e}") from e
        self._session_id = resp.session_id
        logger.info("ACP session/new ok: session_id=%s cwd=%s", self._session_id, cwd)
        return self._session_id

    @property
    def crashed(self) -> bool:
        """True if a prompt failed in a way that poisoned the session/subprocess.

        The manager checks this after every ``prompt`` to decide whether to
        drop the cached client so the next ask respawns a fresh subprocess.
        """
        return self._crashed

    async def prompt(self, text: str, *, timeout: float = 120.0) -> AcpPromptResult:
        """Send a ``session/prompt`` and return the assistant's final turn.

        Streams ``session/update`` notifications into the recorder while waiting
        for ``session/prompt`` to settle. Phase-3 returns the concatenated
        ``agent_message_chunk`` text as the ack reply.

        On timeout: send ``session/cancel`` (best-effort), then close the
        subprocess so a late stream of chunks can't leak into the next ask's
        recorder. The client is marked ``crashed`` and the manager drops it.

        On any transport / protocol failure: same — close + mark crashed.
        Phase-3 favours respawn-on-error over silently reusing a wedged
        connection, since asks are cheap to retry.
        """
        import acp

        async with self._lock:
            if self._closed:
                raise AcpClientError("client is closed")
            await self._ensure_started()
            session_id = await self._ensure_session()
            assert self._connection is not None

            self._recorder.updates.clear()

            try:
                resp = await asyncio.wait_for(
                    self._connection.prompt(
                        prompt=[acp.text_block(text)],
                        session_id=session_id,
                    ),
                    timeout=timeout,
                )
            except asyncio.TimeoutError as e:
                await self._abort_after_failure(cancel_first=True)
                raise AcpClientError(f"prompt timed out after {timeout}s") from e
            except Exception as e:
                await self._abort_after_failure(cancel_first=False)
                raise AcpClientError(f"prompt failed: {e}") from e

            # session/update notifications arrive on the same stdio stream as
            # the session/prompt response, but the SDK dispatches them as
            # separate asyncio tasks. The prompt future can resolve before all
            # pending notification handlers have run, especially against a
            # fast agent like our echo stub. Drain the loop briefly so any
            # in-flight `session/update` is recorded before we read it.
            await _drain_pending_updates(self._recorder)

            assembled = _assemble_agent_text(self._recorder.updates)
            return AcpPromptResult(
                text=assembled,
                stop_reason=str(resp.stop_reason),
                updates=list(self._recorder.updates),
            )

    async def _abort_after_failure(self, *, cancel_first: bool) -> None:
        """Mark the client crashed and tear it down so the manager drops it.

        ``cancel_first`` covers the timeout case: send ``session/cancel`` so
        the agent stops working before we drop its stdio. For protocol errors
        we skip the cancel — the connection is already in a bad state.
        """
        self._crashed = True
        if cancel_first:
            try:
                if self._connection is not None and self._session_id is not None:
                    await self._connection.cancel(self._session_id)
            except Exception as e:  # noqa: BLE001
                logger.warning("ACP cancel during abort failed: %s", e)
        try:
            await self._close_locked()
        except Exception as e:  # noqa: BLE001
            logger.warning("ACP close during abort failed: %s", e)

    async def cancel(self) -> None:
        """Send ``session/cancel`` for the current session, if one exists."""
        if self._connection is None or self._session_id is None:
            return
        try:
            await self._connection.cancel(self._session_id)
        except Exception as e:  # noqa: BLE001 — best-effort cancel
            logger.warning("ACP session/cancel failed: %s", e)

    async def close(self) -> None:
        """Tear down the subprocess and clear session state. Idempotent."""
        await self._close_locked()

    async def _close_locked(self) -> None:
        """Inner close used both from ``close()`` and from the prompt lock.

        Idempotent. Does NOT acquire ``self._lock`` so it's safe to call while
        the prompt path already holds it.
        """
        if self._closed:
            return
        self._closed = True
        stack, self._exit_stack = self._exit_stack, None
        self._connection = None
        self._session_id = None
        if stack is not None:
            try:
                await stack.__aexit__(None, None, None)
            except Exception as e:  # noqa: BLE001
                logger.warning("ACP teardown error: %s", e)


async def _drain_pending_updates(
    recorder: Any,
    *,
    quiescent_ticks: int = 4,
    max_wait_seconds: float = 0.25,
    sleep_step: float = 0.01,
) -> None:
    """Yield control until in-flight session/update tasks have all recorded.

    The SDK dispatches each incoming JSON-RPC frame as a separate task, so a
    session/prompt response future can resolve before sibling session/update
    handlers — and even before the notification frame has been read off the
    pipe in the first place. We poll the recorder for new updates, exiting
    when ``quiescent_ticks`` consecutive polls observe no growth, or when
    ``max_wait_seconds`` elapses.

    The bound is intentionally small: phase-3 is interested in the assistant
    reply that was already streamed by the time stop_reason arrived. Long
    tails belong to later phases (chat_turn_delta streaming).
    """
    deadline = asyncio.get_running_loop().time() + max_wait_seconds
    quiet = 0
    last = len(recorder.updates)
    while asyncio.get_running_loop().time() < deadline:
        await asyncio.sleep(sleep_step)
        current = len(recorder.updates)
        if current == last:
            quiet += 1
            if quiet >= quiescent_ticks:
                return
        else:
            quiet = 0
            last = current


def _assemble_agent_text(updates: list[Any]) -> str:
    """Concatenate ``agent_message_chunk`` text payloads from a list of updates."""
    parts: list[str] = []
    for u in updates:
        if type(u).__name__ != "AgentMessageChunk":
            continue
        content = getattr(u, "content", None)
        text = getattr(content, "text", None) if content is not None else None
        if text:
            parts.append(text)
    return "".join(parts)


def _select_allowed_option(options: list[Any], decision: PermissionDecision) -> Any | None:
    """Return the ACP option selected by a broker decision."""
    if not decision.option_id:
        return options[0] if options else None
    for option in options:
        if getattr(option, "option_id", None) == decision.option_id:
            return option
    return None
