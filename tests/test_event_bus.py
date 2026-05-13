"""Tests for the in-process peer event bus.

Scope (b): typed pub/sub for peer status/liveness transitions. Bounded.
"""

import asyncio
from unittest.mock import AsyncMock, MagicMock

import pytest

from repowire.config.models import Config
from repowire.daemon.event_bus import (
    EventBus,
    PeerEvent,
    PeerStatusChanged,
)
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.peers import Peer, PeerStatus


@pytest.fixture
def bus() -> EventBus:
    return EventBus()


class TestPublishSubscribe:
    async def test_subscribe_receives_published_event(self, bus: EventBus) -> None:
        received: list[PeerEvent] = []

        async def handler(evt: PeerEvent) -> None:
            received.append(evt)

        bus.subscribe(handler)
        evt = PeerStatusChanged(
            peer_id="sid-1",
            display_name="alpha",
            old_status=PeerStatus.OFFLINE,
            new_status=PeerStatus.ONLINE,
        )
        bus.publish(evt)
        await asyncio.sleep(0)  # let fire-and-forget task run
        assert received == [evt]

    async def test_multiple_subscribers_all_receive(self, bus: EventBus) -> None:
        a: list[PeerEvent] = []
        b: list[PeerEvent] = []

        async def ha(e: PeerEvent) -> None:
            a.append(e)

        async def hb(e: PeerEvent) -> None:
            b.append(e)

        bus.subscribe(ha)
        bus.subscribe(hb)
        evt = PeerStatusChanged(
            peer_id="sid-1",
            display_name="alpha",
            old_status=PeerStatus.ONLINE,
            new_status=PeerStatus.OFFLINE,
        )
        bus.publish(evt)
        await asyncio.sleep(0)
        assert a == [evt]
        assert b == [evt]

    async def test_publish_with_no_subscribers_is_noop(self, bus: EventBus) -> None:
        evt = PeerStatusChanged(
            peer_id="sid-1",
            display_name="alpha",
            old_status=PeerStatus.OFFLINE,
            new_status=PeerStatus.ONLINE,
        )
        bus.publish(evt)  # must not raise

    async def test_subscriber_exception_does_not_break_others(
        self, bus: EventBus,
    ) -> None:
        received: list[PeerEvent] = []

        async def bad(_e: PeerEvent) -> None:
            raise RuntimeError("boom")

        async def good(e: PeerEvent) -> None:
            received.append(e)

        bus.subscribe(bad)
        bus.subscribe(good)
        evt = PeerStatusChanged(
            peer_id="sid-1",
            display_name="alpha",
            old_status=PeerStatus.OFFLINE,
            new_status=PeerStatus.ONLINE,
        )
        bus.publish(evt)
        await asyncio.sleep(0)
        assert received == [evt]

    async def test_unsubscribe_stops_delivery(self, bus: EventBus) -> None:
        received: list[PeerEvent] = []

        async def handler(e: PeerEvent) -> None:
            received.append(e)

        token = bus.subscribe(handler)
        bus.unsubscribe(token)
        evt = PeerStatusChanged(
            peer_id="sid-1",
            display_name="alpha",
            old_status=PeerStatus.OFFLINE,
            new_status=PeerStatus.ONLINE,
        )
        bus.publish(evt)
        await asyncio.sleep(0)
        assert received == []


class TestPeerStatusChangedEvent:
    def test_event_carries_transition(self) -> None:
        evt = PeerStatusChanged(
            peer_id="sid-x",
            display_name="x",
            old_status=PeerStatus.BUSY,
            new_status=PeerStatus.OFFLINE,
        )
        assert evt.peer_id == "sid-x"
        assert evt.display_name == "x"
        assert evt.old_status == PeerStatus.BUSY
        assert evt.new_status == PeerStatus.OFFLINE


# ---------------------------------------------------------------------------
# Integration: PeerRegistry emits PeerStatusChanged
# ---------------------------------------------------------------------------


@pytest.fixture
def mock_router() -> MessageRouter:
    router = MagicMock(spec=MessageRouter)
    router.send_query = AsyncMock(return_value="ok")
    router.send_notification = AsyncMock()
    router.broadcast = AsyncMock(return_value=[])
    return router


@pytest.fixture
def registry_with_bus(mock_router, tmp_path) -> tuple[PeerRegistry, EventBus, list[PeerEvent]]:
    bus = EventBus()
    received: list[PeerEvent] = []

    async def collect(evt: PeerEvent) -> None:
        received.append(evt)

    bus.subscribe(collect)
    registry = PeerRegistry(
        config=Config(),
        message_router=mock_router,
        persistence_path=tmp_path / "sessions.json",
        event_bus=bus,
    )
    return registry, bus, received


class TestRegistryEmitsStatusChange:
    async def test_register_peer_emits_online(self, registry_with_bus) -> None:
        registry, _bus, received = registry_with_bus
        peer = Peer(
            peer_id="sid-1",
            display_name="alpha",
            circle="global",
            status=PeerStatus.OFFLINE,
            path="/x",
            machine="m",
        )
        await registry.register_peer(peer)
        await asyncio.sleep(0)
        assert len(received) == 1
        evt = received[0]
        assert isinstance(evt, PeerStatusChanged)
        assert evt.peer_id == "sid-1"
        assert evt.old_status == PeerStatus.OFFLINE
        assert evt.new_status == PeerStatus.ONLINE

    async def test_update_peer_status_emits_change(self, registry_with_bus) -> None:
        registry, _bus, received = registry_with_bus
        peer = Peer(
            peer_id="sid-2", display_name="beta", circle="global",
            status=PeerStatus.OFFLINE, path="/x", machine="m",
        )
        await registry.register_peer(peer)
        await asyncio.sleep(0)
        received.clear()

        await registry.update_peer_status("sid-2", PeerStatus.BUSY)
        await asyncio.sleep(0)
        assert len(received) == 1
        assert received[0].old_status == PeerStatus.ONLINE
        assert received[0].new_status == PeerStatus.BUSY

    async def test_update_peer_status_no_op_does_not_emit(
        self, registry_with_bus,
    ) -> None:
        registry, _bus, received = registry_with_bus
        peer = Peer(
            peer_id="sid-3", display_name="gamma", circle="global",
            status=PeerStatus.OFFLINE, path="/x", machine="m",
        )
        await registry.register_peer(peer)
        await asyncio.sleep(0)
        received.clear()

        await registry.update_peer_status("sid-3", PeerStatus.ONLINE)  # already ONLINE
        await asyncio.sleep(0)
        assert received == []

    async def test_mark_offline_emits_offline(self, registry_with_bus) -> None:
        registry, _bus, received = registry_with_bus
        peer = Peer(
            peer_id="sid-4", display_name="delta", circle="global",
            status=PeerStatus.OFFLINE, path="/x", machine="m",
        )
        await registry.register_peer(peer)
        await asyncio.sleep(0)
        received.clear()

        await registry.mark_offline("sid-4")
        await asyncio.sleep(0)
        assert len(received) == 1
        assert received[0].old_status == PeerStatus.ONLINE
        assert received[0].new_status == PeerStatus.OFFLINE

    async def test_no_bus_means_no_emission(self, mock_router, tmp_path) -> None:
        registry = PeerRegistry(
            config=Config(),
            message_router=mock_router,
            persistence_path=tmp_path / "sessions.json",
            event_bus=None,
        )
        peer = Peer(
            peer_id="sid-5", display_name="epsilon", circle="global",
            status=PeerStatus.OFFLINE, path="/x", machine="m",
        )
        await registry.register_peer(peer)  # must not raise
        await registry.update_peer_status("sid-5", PeerStatus.OFFLINE)
