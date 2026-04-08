"""FastAPI application factory for the Repowire daemon."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import time
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import Depends, FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, HTMLResponse
from fastapi.staticfiles import StaticFiles

from repowire import __version__
from repowire.config.models import Config, load_config
from repowire.daemon.auth import require_localhost
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.lifecycle_handler import LifecycleHandler
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.relay_client import RelayClient
from repowire.daemon.routes import (
    attachments,
    health,
    lifecycle,
    messages,
    peers,
    websocket,
)
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)


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
) -> FastAPI:
    """Create and configure the FastAPI application.

    Args:
        config: Optional configuration. Loaded from disk if not provided.

    Returns:
        Configured FastAPI application.
    """
    _config = config

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        """Manage application startup and shutdown."""
        cfg = _config or load_config()

        # Build the component stack
        _cleanup_stale_artifacts(max_age_hours=cfg.daemon.prune_max_age_hours)
        transport = WebSocketTransport()
        query_tracker = QueryTracker()
        message_router = MessageRouter(
            transport=transport,
            query_tracker=query_tracker,
        )
        peer_registry = PeerRegistry(
            config=cfg,
            message_router=message_router,
            query_tracker=query_tracker,
            transport=transport,
            persistence_path=Path.home() / ".repowire" / "sessions.json",
        )
        peer_registry.prune_offline(max_age_hours=cfg.daemon.prune_max_age_hours)

        # Store in app state for route handlers
        app.state.config = cfg
        app.state.transport = transport
        app.state.query_tracker = query_tracker
        app.state.message_router = message_router
        app.state.peer_registry = peer_registry
        app.state.relay_mode = cfg.relay.enabled

        lifecycle_handler = LifecycleHandler(
            peer_registry=peer_registry,
            query_tracker=query_tracker,
            transport=transport,
        )

        await peer_registry.start()
        init_deps(
            cfg, peer_registry, app.state,
            lifecycle_handler=lifecycle_handler,
        )

        # Install tmux lifecycle hooks if tmux is available
        try:
            from repowire.hooks.tmux_lifecycle import install_hooks, is_tmux_available

            if is_tmux_available():
                tmux_hooks = install_hooks(cfg.daemon.host, cfg.daemon.port)
                if tmux_hooks:
                    logger.info("Installed %d tmux lifecycle hooks", len(tmux_hooks))
        except Exception:
            logger.debug("Tmux hooks not installed", exc_info=True)

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

        yield

        for name, svc in reversed(services):
            await svc.stop()  # type: ignore[union-attr]
            logger.info("%s service stopped", name)
        if relay_client:
            await relay_client.stop()
        peer_registry._save_events()
        peer_registry._persist_mappings()
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
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(websocket.router)
    app.include_router(spawn_routes.router)
    app.include_router(attachments.router)
    app.include_router(lifecycle.router)

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
        async def serve_landing():
            index_path = os.path.join(web_out, "index.html")
            if os.path.exists(index_path):
                return FileResponse(index_path)
            return HTMLResponse("Landing page not found. Please run 'repowire build-ui'.")

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
        msg_router = message_router or MessageRouter(
            transport=transport,
            query_tracker=query_tracker,
        )

        registry = PeerRegistry(
            config=cfg,
            message_router=msg_router,
            query_tracker=query_tracker,
            transport=transport,
            persistence_path=persistence_path,
        )

        app.state.config = cfg
        app.state.transport = transport
        app.state.query_tracker = query_tracker
        app.state.message_router = msg_router
        app.state.peer_registry = registry
        app.state.relay_mode = cfg.relay.enabled

        lh = LifecycleHandler(
            peer_registry=registry,
            query_tracker=query_tracker,
            transport=transport,
        )

        await registry.start()
        init_deps(cfg, registry, app.state, lifecycle_handler=lh)

        yield

        await registry.stop()
        cleanup_deps()

    app = FastAPI(
        title="Repowire Daemon (Test)",
        version=__version__,
        lifespan=test_lifespan,
    )

    app.include_router(health.router)
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(websocket.router)
    app.include_router(spawn_routes.router)
    app.include_router(attachments.router)
    app.include_router(lifecycle.router)

    return app


# Allow running as module: python -m repowire.daemon.app
if __name__ == "__main__":
    import uvicorn

    config = load_config()
    app = create_app()
    uvicorn.run(app, host=config.daemon.host, port=config.daemon.port)
