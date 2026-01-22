"""FastAPI application factory for the Repowire daemon."""

from __future__ import annotations

import os
import signal
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from typing import TYPE_CHECKING

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, HTMLResponse
from fastapi.staticfiles import StaticFiles

from repowire.backends import get_backend as get_backend_by_name
from repowire.config.models import Config, load_config
from repowire.daemon.core import PeerManager
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.routes import health, messages, peers

if TYPE_CHECKING:
    from repowire.backends.base import Backend

__version__ = "0.1.0"


def create_app(
    config: Config | None = None,
    backend_factory: Callable[[], Backend] | None = None,
    backend_override: str | None = None,
    relay_mode: bool = False,
) -> FastAPI:
    """Create and configure the FastAPI application.

    Args:
        config: Optional configuration. Loaded from disk if not provided.
        backend_factory: Optional factory function to create the backend.
        backend_override: Override the configured backend (claudemux or opencode).
        relay_mode: Enable relay mode for remote peer communication.

    Returns:
        Configured FastAPI application.
    """
    # Store these for the lifespan closure
    _backend_override = backend_override
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

        # Create backend
        if _backend_factory:
            backend = _backend_factory()
        elif _backend_override:
            backend = get_backend_by_name(_backend_override)
        else:
            backend = get_backend_by_name(cfg.daemon.backend)

        # Create peer manager
        peer_manager = PeerManager(backend, cfg)

        # Store in app state for access
        app.state.config = cfg
        app.state.backend = backend
        app.state.peer_manager = peer_manager
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
        CORSMiddleware,
        allow_origins=["*"],  # Allow all origins for local dev
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # Include routers
    app.include_router(health.router)
    app.include_router(peers.router)
    app.include_router(messages.router)

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
) -> FastAPI:
    """Create app for testing with optional mock backend."""

    @asynccontextmanager
    async def test_lifespan(app: FastAPI) -> AsyncIterator[None]:
        cfg = config or Config()

        if backend:
            be = backend
        else:
            be = get_backend_by_name(cfg.daemon.backend)

        peer_manager = PeerManager(be, cfg)

        # Store in app state
        app.state.config = cfg
        app.state.backend = be
        app.state.peer_manager = peer_manager
        app.state.relay_mode = cfg.relay.enabled

        await peer_manager.start()
        init_deps(cfg, be, peer_manager)

        yield

        await peer_manager.stop()
        cleanup_deps()

    app = FastAPI(
        title="Repowire Daemon (Test)",
        version=__version__,
        lifespan=test_lifespan,
    )

    app.include_router(health.router)
    app.include_router(peers.router)
    app.include_router(messages.router)

    return app


# Allow running as module: python -m repowire.daemon.app
if __name__ == "__main__":
    import uvicorn

    config = load_config()
    app = create_app()
    uvicorn.run(app, host=config.daemon.host, port=config.daemon.port)
