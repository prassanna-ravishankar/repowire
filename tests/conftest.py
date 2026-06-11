"""Shared test helpers for daemon route/client tests."""

from __future__ import annotations

import time
from collections.abc import Iterable
from dataclasses import dataclass
from types import SimpleNamespace
from typing import Any

import pytest
from fastapi import APIRouter, FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.ask_many import AskManyTracker
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.delivery_trace import DeliveryTraceStore
from repowire.daemon.deps import init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.websocket_transport import WebSocketTransport


@pytest.fixture(autouse=True)
def _hermetic_pane_claim_probes(monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep the destructive pane-claim guard hermetic in tests.

    The guard probes the live host (`ps` ancestry, `tmux` pane pids), and
    tests register fabricated pids/pane ids that can collide with real ones
    on a developer machine. Default both probes to inconclusive (None), which
    lets claims through — the pre-guard behavior. Tests exercising the guard
    monkeypatch these explicitly.
    """
    monkeypatch.setattr(
        "repowire.daemon.peer_registry.process_ancestors", lambda pid: None
    )
    monkeypatch.setattr(
        "repowire.daemon.peer_registry.tmux_pane_pid", lambda pane_id: None
    )


@dataclass
class DaemonAppHarness:
    app: FastAPI
    state: SimpleNamespace
    config: Config
    registry: PeerRegistry
    transport: WebSocketTransport
    query_tracker: QueryTracker
    ask_tracker: AskTracker
    message_router: MessageRouter
    delivery_trace_store: DeliveryTraceStore


def make_daemon_app(
    tmp_path,
    routers: Iterable[APIRouter],
    *,
    config: Config | None = None,
    state_overrides: dict[str, Any] | None = None,
    disable_lazy_repair: bool = True,
) -> DaemonAppHarness:
    """Build a minimal FastAPI daemon app with initialized deps."""
    cfg = config or Config()
    transport = WebSocketTransport()
    query_tracker = QueryTracker()
    ask_tracker = AskTracker(ttl_hours=24.0)
    message_router = MessageRouter(
        transport=transport,
        query_tracker=query_tracker,
    )
    registry = PeerRegistry(
        config=cfg,
        message_router=message_router,
        query_tracker=query_tracker,
        transport=transport,
        ask_tracker=ask_tracker,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    if disable_lazy_repair:
        registry._last_repair = time.monotonic() + 3600

    state_db = StateDatabase(tmp_path / "state.db")
    delivery_trace_store = DeliveryTraceStore(state_db)

    state_kwargs: dict[str, Any] = {
        "config": cfg,
        "transport": transport,
        "query_tracker": query_tracker,
        "ask_tracker": ask_tracker,
        "ask_many_tracker": AskManyTracker(ask_tracker),
        "message_router": message_router,
        "peer_registry": registry,
        "relay_mode": cfg.relay.enabled,
        "delivery_trace_store": delivery_trace_store,
    }
    if state_overrides:
        state_kwargs.update(state_overrides)
    state = SimpleNamespace(**state_kwargs)
    init_deps(cfg, registry, state)

    app = FastAPI()
    for router in routers:
        app.include_router(router)

    return DaemonAppHarness(
        app=app,
        state=state,
        config=cfg,
        registry=registry,
        transport=transport,
        query_tracker=query_tracker,
        ask_tracker=ask_tracker,
        message_router=message_router,
        delivery_trace_store=delivery_trace_store,
    )


def async_client_for(app: FastAPI, *, base_url: str = "http://test") -> AsyncClient:
    return AsyncClient(transport=ASGITransport(app=app), base_url=base_url)
