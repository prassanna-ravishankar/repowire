"""FastAPI application factory for the Repowire daemon."""

from __future__ import annotations

import logging
import os
import signal
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from typing import TYPE_CHECKING, Any

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, HTMLResponse
from fastapi.staticfiles import StaticFiles

from repowire.backends import get_backend as get_backend_by_name
from repowire.config.models import Config, load_config
from repowire.daemon.core import PeerManager, SharedResources
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.routes import health, messages, peers, websocket
from repowire.daemon.websocket_manager import init_ws_manager

if TYPE_CHECKING:
    from repowire.backends.base import Backend

logger = logging.getLogger(__name__)

__version__ = "0.1.0"


def _try_create_backend(name: str, **kwargs: Any) -> Backend | None:
    """Try to create a backend, returning None if it fails.

    Args:
        name: Backend name ("claudemux" or "opencode")
        **kwargs: Additional arguments passed to backend constructor

    Returns:
        Backend instance or None if creation failed
    """
    try:
        return get_backend_by_name(name, **kwargs)
    except ImportError as e:
        logger.debug(f"Backend {name} not available (missing dependency): {e}")
        return None
    except ValueError as e:
        logger.warning(f"Backend {name} failed to initialize: {e}")
        return None
    # Let other exceptions propagate - they indicate real problems that should fail fast


def create_app(
    config: Config | None = None,
    backend_factory: Callable[[], Backend] | None = None,
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

        # Initialize WebSocket manager first (needed for SharedResources)
        ws_manager = init_ws_manager()

        # Create backends and peer manager
        if _backend_factory:
            # Legacy mode: use single backend from factory
            backend = _backend_factory()
            peer_manager = PeerManager(backend=backend, config=cfg)
            shared = None
        else:
            # Per-peer routing mode: create both backends
            claudemux_backend = _try_create_backend("claudemux")
            opencode_backend = _try_create_backend("opencode", ws_manager=ws_manager)

            # Create shared resources for per-peer routing
            shared = SharedResources(
                ws_manager=ws_manager,
                claudemux_backend=claudemux_backend,
                opencode_backend=opencode_backend,
            )

            peer_manager = PeerManager(config=cfg, shared=shared)
            backend = None  # No single backend in per-peer mode

            logger.info(
                f"Per-peer routing enabled: claudemux={claudemux_backend is not None}, "
                f"opencode={opencode_backend is not None}"
            )

        # Store in app state for access
        app.state.config = cfg
        app.state.backend = backend
        app.state.shared = shared
        app.state.peer_manager = peer_manager
        app.state.ws_manager = ws_manager
        app.state.relay_mode = _relay_mode or cfg.relay.enabled

        # Initialize
        await peer_manager.start()
        init_deps(cfg, backend, peer_manager)

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
    app.add_middleware(
        CORSMiddleware,  # type: ignore[invalid-argument-type]
        allow_origins=["*"],  # Allow all origins for local dev
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
    backend: Backend | None = None,
    shared: SharedResources | None = None,
) -> FastAPI:
    """Create app for testing with optional mock backend or shared resources.

    Args:
        config: Optional configuration
        backend: Legacy single backend for testing (mutually exclusive with shared)
        shared: SharedResources for per-peer routing tests
    """

    @asynccontextmanager
    async def test_lifespan(app: FastAPI) -> AsyncIterator[None]:
        cfg = config or Config()
        ws_manager = init_ws_manager()

        if shared:
            # Per-peer routing mode
            pm = PeerManager(config=cfg, shared=shared)
            be = None
        elif backend:
            # Legacy single-backend mode
            pm = PeerManager(backend=backend, config=cfg)
            be = backend
        else:
            # Default: create single backend from config
            be = get_backend_by_name(cfg.daemon.backend)
            pm = PeerManager(backend=be, config=cfg)

        # Store in app state
        app.state.config = cfg
        app.state.backend = be
        app.state.shared = shared
        app.state.peer_manager = pm
        app.state.ws_manager = ws_manager
        app.state.relay_mode = cfg.relay.enabled

        await pm.start()
        init_deps(cfg, be, pm)

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
