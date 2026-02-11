"""FastAPI application factory for the Repowire daemon."""

from __future__ import annotations

import logging
import os
import signal
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, HTMLResponse
from fastapi.staticfiles import StaticFiles

from repowire.config.models import Config, load_config
from repowire.daemon.core import PeerManager
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import health, messages, peers, websocket
from repowire.daemon.session_mapper import SessionMapper
from repowire.daemon.websocket_connection_manager import WebSocketConnectionManager
from repowire.daemon.websocket_transport import WebSocketTransport

logger = logging.getLogger(__name__)

__version__ = "0.1.0"


def create_app(
    config: Config | None = None,
    backend_factory: Callable[[], Any] | None = None,
    relay_mode: bool = False,
) -> FastAPI:
    """Create and configure the FastAPI application.

    Args:
        config: Optional configuration. Loaded from disk if not provided.
        backend_factory: Optional factory function to create the backend.
        relay_mode: Enable relay mode for remote peer communication.

    Returns:
        Configured FastAPI application.
    """
    # Store these for the lifespan closure
    _relay_mode = relay_mode
    _backend_factory = backend_factory
    _config = config

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        """Manage application startup and shutdown."""
        # Load config
        cfg = _config or load_config()

        # Apply relay mode override
        if _relay_mode:
            cfg.relay.enabled = True

        # Layer 0: Session Mapping
        session_mapper = SessionMapper(
            persistence_path=Path.home() / ".repowire" / "sessions.json"
        )

        # Layer 1: Transport
        transport = WebSocketTransport()

        # Layer 2: Connection Manager
        conn_mgr = WebSocketConnectionManager()

        # Layer 3: Query Tracker
        query_tracker = QueryTracker()

        # Layer 4: Message Router
        message_router = MessageRouter(
            transport=transport,
            connection_manager=conn_mgr,
            query_tracker=query_tracker,
        )

        # Simplified PeerManager
        peer_manager = PeerManager(
            config=cfg,
            message_router=message_router,
            session_mapper=session_mapper,
        )

        # Store in app state for access
        app.state.config = cfg
        app.state.session_mapper = session_mapper
        app.state.transport = transport
        app.state.conn_mgr = conn_mgr
        app.state.query_tracker = query_tracker
        app.state.message_router = message_router
        app.state.peer_manager = peer_manager
        app.state.relay_mode = _relay_mode or cfg.relay.enabled

        # Initialize
        await peer_manager.start()
        init_deps(cfg, None, peer_manager, app.state)

        logger.info("Unified WebSocket backend initialized")

        yield

        # Cleanup
        await peer_manager.stop()
        cleanup_deps()

    app = FastAPI(
        title="Repowire Daemon",
        description="HTTP daemon for the Repowire mesh network",
        version=__version__,
        lifespan=lifespan,
    )

    # CORS middleware for local development
    # Restrict to localhost origins to prevent CSRF attacks
    app.add_middleware(
        CORSMiddleware,  # type: ignore[invalid-argument-type]
        allow_origins=[
            "http://localhost:3000",
            "http://127.0.0.1:3000",
            "http://localhost:8377",
            "http://127.0.0.1:8377",
        ],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # Include routers
    app.include_router(health.router)
    app.include_router(peers.router)
    app.include_router(messages.router)
    app.include_router(websocket.router)

    # --- Static File Serving (Dashboard) ---
    # Find the web output directory - check multiple locations
    web_out = None

    # 1. Dev mode: relative to repo root (3 dirs up from app.py)
    dev_base = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    dev_web_out = os.path.join(dev_base, "web", "out")

    # 2. Installed mode: web/out is sibling to repowire package in site-packages
    import sys

    for path in sys.path:
        installed_web_out = os.path.join(path, "web", "out")
        if os.path.exists(installed_web_out) and os.path.isfile(
            os.path.join(installed_web_out, "dashboard.html")
        ):
            web_out = installed_web_out
            break

    # Prefer dev mode if available (for local development)
    if os.path.exists(dev_web_out) and os.path.isfile(os.path.join(dev_web_out, "dashboard.html")):
        web_out = dev_web_out

    if web_out and os.path.exists(web_out):
        # Mount the _next directory for assets
        next_static = os.path.join(web_out, "_next")
        if os.path.exists(next_static):
            app.mount("/_next", StaticFiles(directory=next_static), name="next_static")

        # Serve specific routes
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

        # Mount the rest of the static files (images, icons, etc.)
        app.mount("/", StaticFiles(directory=web_out), name="web_static")

    # Add shutdown endpoint
    @app.post("/shutdown", include_in_schema=False)
    async def shutdown():
        """Shutdown the daemon gracefully."""
        import asyncio

        loop = asyncio.get_event_loop()
        loop.call_later(0.5, lambda: os.kill(os.getpid(), signal.SIGTERM))
        return {"status": "shutting_down"}

    return app


def create_test_app(
    config: Config | None = None,
    session_mapper: SessionMapper | None = None,
    message_router: MessageRouter | None = None,
) -> FastAPI:
    """Create app for testing with optional mock components.

    Args:
        config: Optional configuration
        session_mapper: Optional SessionMapper for testing
        message_router: Optional MessageRouter for testing
    """

    @asynccontextmanager
    async def test_lifespan(app: FastAPI) -> AsyncIterator[None]:
        cfg = config or Config()

        # Use provided components or create new ones
        sess_mapper = session_mapper or SessionMapper(
            persistence_path=Path.home() / ".repowire" / "test-sessions.json"
        )
        transport = WebSocketTransport()
        conn_mgr = WebSocketConnectionManager()
        query_tracker = QueryTracker()
        msg_router = message_router or MessageRouter(
            transport=transport,
            connection_manager=conn_mgr,
            query_tracker=query_tracker,
        )

        pm = PeerManager(
            config=cfg,
            message_router=msg_router,
            session_mapper=sess_mapper,
        )

        # Store in app state
        app.state.config = cfg
        app.state.session_mapper = sess_mapper
        app.state.transport = transport
        app.state.conn_mgr = conn_mgr
        app.state.query_tracker = query_tracker
        app.state.message_router = msg_router
        app.state.peer_manager = pm
        app.state.relay_mode = cfg.relay.enabled

        await pm.start()
        init_deps(cfg, None, pm, app.state)

        yield

        await pm.stop()
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

    return app


# Allow running as module: python -m repowire.daemon.app
if __name__ == "__main__":
    import uvicorn

    config = load_config()
    app = create_app()
    uvicorn.run(app, host=config.daemon.host, port=config.daemon.port)
