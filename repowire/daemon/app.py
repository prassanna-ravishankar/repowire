"""FastAPI application factory for the Repowire daemon."""

from __future__ import annotations

import asyncio
import logging
import os
import secrets
import signal
import time
from collections.abc import AsyncIterator
from contextlib import AsyncExitStack, asynccontextmanager
from pathlib import Path

from fastapi import Depends, FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, HTMLResponse
from fastapi.staticfiles import StaticFiles
from starlette.middleware.base import RequestResponseEndpoint
from starlette.requests import Request
from starlette.responses import Response
from starlette.types import ASGIApp, Receive, Scope, Send

from repowire import __version__
from repowire.agent_types import AgentType
from repowire.config.models import Config, load_config
from repowire.daemon.acp_reconcile import reconcile_acp_inflight
from repowire.daemon.ask_many import AskManyTracker
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.auth import require_localhost
from repowire.daemon.delivery_trace import DeliveryTraceStore
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.event_bus import EventBus
from repowire.daemon.event_log import EventLog
from repowire.daemon.job_completion import JobCompletionService
from repowire.daemon.job_runner import JobRunner
from repowire.daemon.lifecycle_handler import LifecycleHandler
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_delivery import PeerDeliveryService
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.relay_client import RelayClient
from repowire.daemon.review_queue_store import ReviewQueueStore
from repowire.daemon.routes import (
    acp_permissions,
    asks,
    attachments,
    health,
    lifecycle,
    messages,
    peers,
    reviews,
    schedules,
    sessions,
    traces,
    websocket,
    work,
)
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.scheduler import Scheduler
from repowire.daemon.session_control import SessionControlService
from repowire.daemon.spawn_service import SpawnService
from repowire.daemon.state import StateDatabase
from repowire.daemon.state.calendar import SQLiteCalendarStore
from repowire.daemon.state.events import SQLiteEventStore
from repowire.daemon.state.operations import SQLiteOperationStore
from repowire.daemon.state.queued_deliveries import SQLiteQueuedDeliveryStore
from repowire.daemon.state.schedules import SQLiteScheduleStore
from repowire.daemon.state.session_bindings import (
    SQLiteSessionBindingStore,
    resolve_repowire_session_id,
)
from repowire.daemon.state.work import SQLiteWorkStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import PeerRole

logger = logging.getLogger(__name__)


_LOCALHOST_HOSTS = {"127.0.0.1", "::1", "localhost"}


def _make_acp_session_resolver(
    session_binding_store: SQLiteSessionBindingStore | None,
):
    def _resolve(peer_id: str, runtime_session_id: str) -> str | None:
        return resolve_repowire_session_id(
            session_binding_store,
            peer_id=peer_id,
            runtime_session_id=runtime_session_id,
        )

    return _resolve


class _MCPAuthASGIWrapper:
    """Bearer-auth wrapper for the mounted Streamable HTTP MCP app."""

    def __init__(self, app: ASGIApp, token: str | None, require_auth: bool) -> None:
        self.app = app
        self.token = token
        self.require_auth = require_auth

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] not in {"http", "websocket"}:
            await self.app(scope, receive, send)
            return
        if self.require_auth:
            if not self.token:
                await self._reject(
                    send,
                    503,
                    (
                        b"HTTP MCP auth is enabled but daemon.auth_token is not set; "
                        b"run `repowire setup --http-mcp` or set daemon.auth_token"
                    ),
                )
                return
            headers = {k.lower(): v for k, v in scope.get("headers", [])}
            expected = f"Bearer {self.token}"
            provided = headers.get(b"authorization", b"").decode("utf-8", errors="ignore")
            if not secrets.compare_digest(provided, expected):
                await self._reject(
                    send,
                    401,
                    b"Missing or invalid bearer token for localhost HTTP MCP",
                )
                return
        if scope.get("path") == "":
            scope = dict(scope)
            scope["path"] = "/"
        await self.app(scope, receive, send)

    @staticmethod
    async def _reject(send: Send, status: int, body: bytes) -> None:
        await send({
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", b"text/plain; charset=utf-8"),
                (b"www-authenticate", b"Bearer"),
            ],
        })
        await send({"type": "http.response.body", "body": body})


def _mount_http_mcp(app: FastAPI, cfg: Config) -> None:
    """Mount the opt-in localhost Streamable HTTP MCP endpoint."""
    mcp_cfg = cfg.daemon.mcp_http
    if not mcp_cfg.enabled:
        try:
            from repowire.mcp.server import reset_mcp_context

            reset_mcp_context()
        except Exception:
            logger.debug("Could not reset MCP context", exc_info=True)
        return
    if mcp_cfg.expose_via_relay:
        logger.warning("Ignoring daemon.mcp_http.expose_via_relay=true; /mcp is local-only")
    if mcp_cfg.bind != "localhost-only" or cfg.daemon.host not in _LOCALHOST_HOSTS:
        logger.warning("HTTP MCP not mounted: daemon.mcp_http is localhost-only")
        return
    require_auth = mcp_cfg.require_auth and not mcp_cfg.allow_unauthenticated_localhost
    from repowire.mcp.server import configure_http_mcp_context, create_mcp_server

    configure_http_mcp_context(
        auth_token=cfg.daemon.auth_token,
        allow_dangerous_tools=mcp_cfg.allow_dangerous_tools,
    )
    @app.middleware("http")
    async def mcp_path_middleware(
        request: Request,
        call_next: RequestResponseEndpoint,
    ) -> Response:
        """Route both /mcp and /mcp/ to the mounted MCP app without redirects."""
        if request.scope.get("path") == "/mcp":
            request.scope["path"] = "/mcp/"
        return await call_next(request)

    mcp = create_mcp_server(streamable_http_path="/")
    app.mount(
        "/mcp",
        _MCPAuthASGIWrapper(mcp.streamable_http_app(), cfg.daemon.auth_token, require_auth),
        name="mcp_http",
    )
    app.state.mcp_http_session_manager = mcp.session_manager


def _cleanup_stale_artifacts(max_age_hours: float = 72) -> None:
    """Remove stale PID, log, and lock files from cache directory."""
    from repowire.config.models import CACHE_DIR

    log_dir = CACHE_DIR / "logs"
    if not log_dir.exists():
        return
    cutoff = time.time() - (max_age_hours * 3600)
    for f in log_dir.iterdir():
        try:
            if f.suffix in (".pid", ".log", ".lock") and f.stat().st_mtime < cutoff:
                f.unlink(missing_ok=True)
        except OSError as e:
            logger.warning("Error cleaning up stale artifact %s: %s", f, e)


def create_app(
    config: Config | None = None,
    install_tmux_hooks: bool = True,
) -> FastAPI:
    """Create and configure the FastAPI application.

    Args:
        config: Optional configuration. Loaded from disk if not provided.
        install_tmux_hooks: When False, skip installing server-global tmux
            lifecycle hooks during startup. Smoke/test daemons running under
            a temp HOME should pass False so they don't rewrite the user's
            live tmux hooks (which are not scoped to HOME).

    Returns:
        Configured FastAPI application.
    """
    _config = config
    _install_tmux_hooks = install_tmux_hooks

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        """Manage application startup and shutdown."""
        cfg = _config or load_config()

        # Build the component stack
        _cleanup_stale_artifacts(max_age_hours=cfg.daemon.prune_max_age_hours)
        transport = WebSocketTransport()
        query_tracker = QueryTracker()
        ask_tracker = AskTracker(ttl_hours=cfg.daemon.prune_max_age_hours)
        state_dir = Path.home() / ".repowire"
        events_path = state_dir / "events.json"
        schedules_path = state_dir / "schedules.json"
        state_db = StateDatabase(state_dir / "state.db")
        event_store = SQLiteEventStore(
            db=state_db,
            legacy_path=events_path,
        )
        event_log = EventLog(events_path, store=event_store)
        message_router = MessageRouter(
            transport=transport,
            query_tracker=query_tracker,
        )
        event_bus = EventBus()
        peer_registry = PeerRegistry(
            config=cfg,
            message_router=message_router,
            query_tracker=query_tracker,
            transport=transport,
            persistence_path=Path.home() / ".repowire" / "sessions.json",
            ask_tracker=ask_tracker,
            event_bus=event_bus,
            event_log=event_log,
            state_db=state_db,
        )
        peer_registry.prune_offline(max_age_hours=cfg.daemon.prune_max_age_hours)

        schedule_store = SQLiteScheduleStore(
            db=state_db,
            legacy_path=schedules_path,
        )
        session_binding_store = SQLiteSessionBindingStore(state_db)
        queued_delivery_store = SQLiteQueuedDeliveryStore(
            state_db,
            ttl_seconds=cfg.daemon.delivery_queue_ttl_seconds,
            max_per_peer=cfg.daemon.delivery_queue_max_per_peer,
        )
        work_store = SQLiteWorkStore(state_db)
        calendar_store = SQLiteCalendarStore(state_db, work_store)
        operation_store = SQLiteOperationStore(state_db)
        delivery_trace_store = DeliveryTraceStore(state_db)
        # Store in app state for route handlers
        app.state.config = cfg
        app.state.transport = transport
        app.state.query_tracker = query_tracker
        app.state.ask_tracker = ask_tracker
        app.state.ask_many_tracker = AskManyTracker(ask_tracker)
        app.state.event_log = event_log
        app.state.message_router = message_router
        app.state.peer_registry = peer_registry
        app.state.event_bus = event_bus
        app.state.relay_mode = cfg.relay.enabled
        app.state.review_queue_store = ReviewQueueStore(
            Path.home() / ".repowire" / "review_queue.json"
        )
        app.state.schedule_store = schedule_store
        app.state.work_store = work_store
        app.state.calendar_store = calendar_store
        app.state.operation_store = operation_store
        app.state.state_db = state_db
        app.state.session_binding_store = session_binding_store
        app.state.queued_delivery_store = queued_delivery_store
        app.state.delivery_trace_store = delivery_trace_store
        from repowire.acp import AcpClientManager, ApprovalBroker
        acp_permission_broker = ApprovalBroker(
            emit_event=peer_registry.add_event,
            ask_tracker=ask_tracker,
            resolve_repowire_session_id=_make_acp_session_resolver(session_binding_store),
        )
        app.state.acp_permission_broker = acp_permission_broker
        acp_manager = AcpClientManager(approval_broker=acp_permission_broker)
        app.state.acp_manager = acp_manager
        from repowire.daemon.transport_router import transport_router_from_state
        peer_delivery = PeerDeliveryService(
            registry=peer_registry,
            message_router=message_router,
            transport_router=transport_router_from_state(
                config=cfg,
                registry=peer_registry,
                state=app.state,
            ),
            config=cfg,
            ask_tracker=ask_tracker,
            session_binding_store=session_binding_store,
            queued_delivery_store=queued_delivery_store,
            operation_store=operation_store,
        )
        app.state.peer_delivery = peer_delivery
        spawn_service = SpawnService(
            spawn=cfg.daemon.spawn,
            spawned_pane_ids=spawn_routes._SPAWNED_PANE_IDS,
            background_tasks=spawn_routes._BACKGROUND_TASKS,
        )
        app.state.spawn_service = spawn_service
        session_control = SessionControlService(
            peer_registry=peer_registry,
            spawn_service=spawn_service,
            operation_store=operation_store,
            session_binding_store=session_binding_store,
            calendar_store=calendar_store,
        )
        app.state.session_control = session_control
        job_runner = JobRunner(
            config=cfg,
            work_store=work_store,
            calendar_store=calendar_store,
            peer_registry=peer_registry,
            peer_delivery=peer_delivery,
            spawn_service=spawn_service,
            session_binding_store=session_binding_store,
            session_control=session_control,
        )
        app.state.job_runner = job_runner
        job_completion = JobCompletionService(
            work_store=work_store,
            ask_tracker=ask_tracker,
            session_control=session_control,
            peer_registry=peer_registry,
            peer_delivery=peer_delivery,
        )
        app.state.job_completion = job_completion
        work_store.set_on_state_change(job_completion.on_work_state_change)
        peer_registry.set_terminal_offline_hook(
            lambda peer_id, reason: job_completion.on_peer_terminal_offline(
                peer_id, reason=reason,
            )
        )
        scheduler = Scheduler(
            store=schedule_store,
            peer_registry=peer_registry,
            ask_tracker=ask_tracker,
            peer_delivery=peer_delivery,
        )
        app.state.scheduler = scheduler

        lifecycle_handler = LifecycleHandler(
            peer_registry=peer_registry,
            query_tracker=query_tracker,
            transport=transport,
        )

        await peer_registry.start()

        # Register the in-process @jobs service peer: the addressable sender
        # for job dispatch asks (a real return address instead of a ghost
        # string). It lives exactly as long as the daemon; lazy repair skips
        # in_process peers, and dispatch asks use pull reply delivery so no
        # transport is ever needed.
        try:
            jobs_peer_id, _ = await peer_registry.allocate_and_register(
                circle="default",
                backend=AgentType.MCP_HTTP,
                path="/jobs",
                role=PeerRole.SERVICE,
                metadata={"in_process": True},
                display_name_override="jobs",
            )
            job_runner.set_sender_peer_id(jobs_peer_id)
            job_completion.set_sender_peer_id(jobs_peer_id)
            app.state.jobs_peer_id = jobs_peer_id
        except Exception:
            logger.exception("Failed to register @jobs service peer")

        await scheduler.start()
        await job_runner.start()
        # Post-restart: fail in-flight fires whose executor died while the
        # daemon was down (grace-delayed so live ws-hooks can reconnect first).
        job_reconcile_task = asyncio.create_task(job_completion.reconcile_inflight())
        init_deps(
            cfg, peer_registry, app.state,
            lifecycle_handler=lifecycle_handler,
        )

        # Fail-loud: an ACP ask in flight when the daemon last stopped lost its
        # background prompt task (and the in-memory AskTracker). Reconcile those
        # durable acp_ask operations now so the asker gets a closure rather than
        # waiting forever.
        reconcile_acp_inflight(operation_store, queued_delivery_store)

        # Install tmux lifecycle hooks if tmux is available.
        # Skipped when the caller disables it (e.g. smoke daemons under a
        # temp HOME), because tmux hooks are server-global and would
        # otherwise rewrite the user's live mesh hooks.
        if _install_tmux_hooks:
            try:
                from repowire.hooks import tmux_lifecycle

                if tmux_lifecycle.is_tmux_available():
                    tmux_hooks = tmux_lifecycle.install_hooks(cfg.daemon.host, cfg.daemon.port)
                    if tmux_hooks:
                        logger.info("Installed %d tmux lifecycle hooks", len(tmux_hooks))
            except Exception:
                logger.debug("Tmux hooks not installed", exc_info=True)
        else:
            logger.info("Skipping tmux lifecycle hook install (install_tmux_hooks=False)")

        # One-shot orphan ws-hook sweep: kill hook processes whose agent is
        # conclusively gone (they otherwise hold websockets open and keep dead
        # peers ONLINE), and retire the peers they were impersonating. Gated
        # with the tmux-hook flag because it touches host-global processes —
        # a smoke daemon must not kill the live mesh's hooks.
        if _install_tmux_hooks:
            try:
                from repowire.hooks.ws_hook_supervisor import sweep_orphan_ws_hooks

                orphan_reports = await asyncio.to_thread(sweep_orphan_ws_hooks)
                for report in orphan_reports:
                    logger.warning(
                        "Swept orphan ws-hook pid=%s pane=%s peer=%s (%s, killed=%s)",
                        report.pid,
                        report.pane_id,
                        report.peer_id,
                        report.reason,
                        report.killed,
                    )
                    if report.killed and report.peer_id:
                        await peer_registry.mark_offline(
                            report.peer_id,
                            reason="orphan_ws_hook",
                            source="startup_sweep",
                            detail=report.reason,
                            terminal=True,
                        )
            except Exception:
                logger.warning("Orphan ws-hook sweep failed", exc_info=True)

        # Start relay client if enabled
        relay_client: RelayClient | None = None
        if cfg.relay.enabled and cfg.relay.api_key:
            local_url = f"http://{cfg.daemon.host}:{cfg.daemon.port}"
            relay_client = RelayClient(config=cfg.relay, local_base_url=local_url)
            await relay_client.start()
            app.state.relay_client = relay_client
            logger.info("Relay client connected to %s", cfg.relay.url)

        # Start configured bot services
        daemon_url = f"http://{cfg.daemon.host}:{cfg.daemon.port}"
        services: list[tuple[str, object]] = []

        if cfg.telegram.bot_token and cfg.telegram.chat_id:
            from repowire.telegram.bot import TelegramPeer

            services.append(("telegram", TelegramPeer(
                bot_token=cfg.telegram.bot_token,
                chat_id=cfg.telegram.chat_id,
                daemon_url=daemon_url,
            )))

        if cfg.slack.bot_token and cfg.slack.app_token and cfg.slack.channel_id:
            from repowire.slack.bot import SlackPeer

            services.append(("slack", SlackPeer(
                bot_token=cfg.slack.bot_token,
                app_token=cfg.slack.app_token,
                channel_id=cfg.slack.channel_id,
                daemon_url=daemon_url,
            )))

        for name, svc in services:
            asyncio.create_task(svc.start())  # type: ignore[union-attr]
            logger.info("%s service started", name)

        logger.info("Unified WebSocket backend initialized")

        mcp_session_manager = getattr(app.state, "mcp_http_session_manager", None)
        async with AsyncExitStack() as stack:
            if mcp_session_manager is not None:
                await stack.enter_async_context(mcp_session_manager.run())
                logger.info("HTTP MCP session manager started")

            yield

        if mcp_session_manager is not None:
            from repowire.mcp.server import reset_mcp_context

            reset_mcp_context()

        for name, svc in reversed(services):
            await svc.stop()  # type: ignore[union-attr]
            logger.info("%s service stopped", name)
        if relay_client:
            await relay_client.stop()
        await acp_manager.close()
        job_reconcile_task.cancel()
        await job_runner.stop()
        await scheduler.stop()
        event_log.save()
        peer_registry._persist_mappings()
        state_db.close()
        await peer_registry.stop()
        cleanup_deps()

    app = FastAPI(
        title="Repowire Daemon",
        description="HTTP daemon for the Repowire mesh network",
        version=__version__,
        lifespan=lifespan,
    )

    # CORS middleware
    cors_origins = [
        "http://localhost:3000",
        "http://127.0.0.1:3000",
        "http://localhost:8377",
        "http://127.0.0.1:8377",
    ]
    if _config and _config.relay.enabled:
        cors_origins.extend(
            [
                "https://repowire.io",
                "https://relay.repowire.io",
            ]
        )
    app.add_middleware(
        CORSMiddleware,  # type: ignore[invalid-argument-type]
        allow_origins=cors_origins,
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # Include routers
    app.include_router(health.router)
    app.include_router(acp_permissions.router)
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(asks.router)
    app.include_router(reviews.router)
    app.include_router(websocket.router)
    app.include_router(spawn_routes.router)
    app.include_router(attachments.router)
    app.include_router(lifecycle.router)
    app.include_router(schedules.router)
    app.include_router(sessions.router)
    app.include_router(traces.router)
    app.include_router(work.router)

    _mount_http_mcp(app, _config or load_config())

    # --- Static File Serving (Dashboard) ---
    web_out = _find_web_output_dir()

    if web_out:
        next_static = os.path.join(web_out, "_next")
        if os.path.exists(next_static):
            app.mount("/_next", StaticFiles(directory=next_static), name="next_static")

        @app.get("/dashboard", response_class=HTMLResponse, include_in_schema=False)
        async def serve_dashboard():
            dashboard_path = os.path.join(web_out, "dashboard.html")
            if os.path.exists(dashboard_path):
                return FileResponse(dashboard_path)
            return HTMLResponse("Dashboard not found. Please run 'repowire build-ui'.")

        @app.get("/", response_class=HTMLResponse, include_in_schema=False)
        async def serve_root():
            # The local daemon is an operator surface: open the dashboard directly
            # rather than the marketing landing page (the relay serves marketing).
            dashboard_path = os.path.join(web_out, "dashboard.html")
            if os.path.exists(dashboard_path):
                return FileResponse(dashboard_path)
            index_path = os.path.join(web_out, "index.html")
            if os.path.exists(index_path):
                return FileResponse(index_path)
            return HTMLResponse("Dashboard not found. Please run 'repowire build-ui'.")

        app.mount("/", StaticFiles(directory=web_out), name="web_static")

    @app.post("/shutdown", include_in_schema=False)
    async def shutdown(_: None = Depends(require_localhost)):
        """Shutdown the daemon gracefully. Restricted to localhost."""
        loop = asyncio.get_event_loop()
        loop.call_later(0.5, lambda: os.kill(os.getpid(), signal.SIGTERM))
        return {"status": "shutting_down"}

    return app


def _find_web_output_dir() -> str | None:
    """Find the web output directory for the dashboard.

    Checks dev mode first (relative to repo root), then installed mode
    (sibling to repowire package in site-packages).
    """
    import sys

    # Dev mode: relative to repo root (3 dirs up from app.py)
    dev_base = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    dev_web_out = os.path.join(dev_base, "web", "out")

    if os.path.exists(dev_web_out) and os.path.isfile(os.path.join(dev_web_out, "dashboard.html")):
        return dev_web_out

    # Installed mode: web/out is sibling to repowire package in site-packages
    for path in sys.path:
        installed_web_out = os.path.join(path, "web", "out")
        if os.path.exists(installed_web_out) and os.path.isfile(
            os.path.join(installed_web_out, "dashboard.html")
        ):
            return installed_web_out

    return None


def create_test_app(
    config: Config | None = None,
    message_router: MessageRouter | None = None,
    persistence_path: Path | None = None,
) -> FastAPI:
    """Create app for testing with optional mock components.

    Args:
        config: Optional configuration
        message_router: Optional MessageRouter for testing
        persistence_path: Optional path for session persistence
    """

    @asynccontextmanager
    async def test_lifespan(app: FastAPI) -> AsyncIterator[None]:
        cfg = config or Config()

        transport = WebSocketTransport()
        query_tracker = QueryTracker()
        ask_tracker = AskTracker(ttl_hours=cfg.daemon.prune_max_age_hours)
        state_dir = persistence_path.parent if persistence_path else Path("/tmp")
        event_path = state_dir / "events.json"
        state_db = StateDatabase(state_dir / "state.db")
        event_store = SQLiteEventStore(
            db=state_db,
            legacy_path=event_path,
        )
        event_log = EventLog(event_path, store=event_store)
        msg_router = message_router or MessageRouter(
            transport=transport,
            query_tracker=query_tracker,
        )

        event_bus = EventBus()
        schedules_path = state_dir / "schedules.json"
        registry = PeerRegistry(
            config=cfg,
            message_router=msg_router,
            query_tracker=query_tracker,
            transport=transport,
            persistence_path=persistence_path,
            ask_tracker=ask_tracker,
            event_bus=event_bus,
            event_log=event_log,
            state_db=state_db,
        )
        schedule_store = SQLiteScheduleStore(
            db=state_db,
            legacy_path=schedules_path,
        )
        session_binding_store = SQLiteSessionBindingStore(state_db)
        queued_delivery_store = SQLiteQueuedDeliveryStore(
            state_db,
            ttl_seconds=cfg.daemon.delivery_queue_ttl_seconds,
            max_per_peer=cfg.daemon.delivery_queue_max_per_peer,
        )
        work_store = SQLiteWorkStore(state_db)
        calendar_store = SQLiteCalendarStore(state_db, work_store)
        operation_store = SQLiteOperationStore(state_db)
        delivery_trace_store = DeliveryTraceStore(state_db)
        app.state.config = cfg
        app.state.transport = transport
        app.state.query_tracker = query_tracker
        app.state.ask_tracker = ask_tracker
        app.state.ask_many_tracker = AskManyTracker(ask_tracker)
        app.state.event_log = event_log
        app.state.message_router = msg_router
        app.state.peer_registry = registry
        app.state.event_bus = event_bus
        app.state.relay_mode = cfg.relay.enabled
        rq_dir = persistence_path.parent if persistence_path else Path.home() / ".repowire"
        app.state.review_queue_store = ReviewQueueStore(rq_dir / "review_queue.json")
        app.state.schedule_store = schedule_store
        app.state.work_store = work_store
        app.state.calendar_store = calendar_store
        app.state.operation_store = operation_store
        app.state.state_db = state_db
        app.state.session_binding_store = session_binding_store
        app.state.queued_delivery_store = queued_delivery_store
        app.state.delivery_trace_store = delivery_trace_store
        from repowire.acp import AcpClientManager, ApprovalBroker
        acp_permission_broker = ApprovalBroker(
            emit_event=registry.add_event,
            ask_tracker=ask_tracker,
            resolve_repowire_session_id=_make_acp_session_resolver(session_binding_store),
        )
        app.state.acp_permission_broker = acp_permission_broker
        acp_manager = AcpClientManager(approval_broker=acp_permission_broker)
        app.state.acp_manager = acp_manager
        from repowire.daemon.transport_router import transport_router_from_state
        peer_delivery = PeerDeliveryService(
            registry=registry,
            message_router=msg_router,
            transport_router=transport_router_from_state(
                config=cfg,
                registry=registry,
                state=app.state,
            ),
            config=cfg,
            ask_tracker=ask_tracker,
            session_binding_store=session_binding_store,
            queued_delivery_store=queued_delivery_store,
            operation_store=operation_store,
        )
        app.state.peer_delivery = peer_delivery
        spawn_service = SpawnService(
            spawn=cfg.daemon.spawn,
            spawned_pane_ids=spawn_routes._SPAWNED_PANE_IDS,
            background_tasks=spawn_routes._BACKGROUND_TASKS,
        )
        app.state.spawn_service = spawn_service
        session_control = SessionControlService(
            peer_registry=registry,
            spawn_service=spawn_service,
            operation_store=operation_store,
            session_binding_store=session_binding_store,
            calendar_store=calendar_store,
        )
        app.state.session_control = session_control
        job_runner = JobRunner(
            config=cfg,
            work_store=work_store,
            calendar_store=calendar_store,
            peer_registry=registry,
            peer_delivery=peer_delivery,
            spawn_service=spawn_service,
            session_binding_store=session_binding_store,
            session_control=session_control,
        )
        app.state.job_runner = job_runner
        scheduler = Scheduler(
            store=schedule_store,
            peer_registry=registry,
            ask_tracker=ask_tracker,
            peer_delivery=peer_delivery,
        )
        app.state.scheduler = scheduler

        lh = LifecycleHandler(
            peer_registry=registry,
            query_tracker=query_tracker,
            transport=transport,
        )

        await registry.start()
        await scheduler.start()
        init_deps(cfg, registry, app.state, lifecycle_handler=lh)
        reconcile_acp_inflight(operation_store, queued_delivery_store)

        mcp_session_manager = getattr(app.state, "mcp_http_session_manager", None)
        async with AsyncExitStack() as stack:
            if mcp_session_manager is not None:
                await stack.enter_async_context(mcp_session_manager.run())

            yield

        if mcp_session_manager is not None:
            from repowire.mcp.server import reset_mcp_context

            reset_mcp_context()

        await acp_manager.close()
        await scheduler.stop()
        event_log.save()
        registry._persist_mappings()
        state_db.close()
        await registry.stop()
        cleanup_deps()

    app = FastAPI(
        title="Repowire Daemon (Test)",
        version=__version__,
        lifespan=test_lifespan,
    )

    app.include_router(health.router)
    app.include_router(acp_permissions.router)
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(asks.router)
    app.include_router(reviews.router)
    app.include_router(websocket.router)
    app.include_router(spawn_routes.router)
    app.include_router(attachments.router)
    app.include_router(lifecycle.router)
    app.include_router(schedules.router)
    app.include_router(sessions.router)
    app.include_router(traces.router)
    app.include_router(work.router)

    _mount_http_mcp(app, config or Config())

    return app


# Allow running as module: python -m repowire.daemon.app
if __name__ == "__main__":
    import uvicorn

    config = load_config()
    app = create_app()
    uvicorn.run(app, host=config.daemon.host, port=config.daemon.port)
