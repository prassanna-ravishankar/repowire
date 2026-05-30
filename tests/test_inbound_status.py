"""Table-driven tests for compute_inbound_status precedence."""

from __future__ import annotations

import pytest

from repowire.daemon.diagnostics import (
    INBOUND_DEGRADED,
    INBOUND_LEGACY_UNVERIFIED,
    INBOUND_NO_HOOK,
    INBOUND_OFFLINE,
    INBOUND_ONLINE,
    INBOUND_PANE_UNSAFE,
    compute_inbound_status,
)


@pytest.mark.parametrize(
    "kwargs,expected",
    [
        # offline short-circuits everything
        (dict(is_offline=True, ws_connected=True, hook_supports_receipts=True), INBOUND_OFFLINE),
        # pane_unsafe only when actually probed False AND connected
        (
            dict(is_offline=False, ws_connected=True, hook_supports_receipts=True, pane_safe=False),
            INBOUND_PANE_UNSAFE,
        ),
        # pane_safe=None (unprobed) must NOT assert pane_unsafe
        (
            dict(is_offline=False, ws_connected=True, hook_supports_receipts=True, pane_safe=None),
            INBOUND_ONLINE,
        ),
        # no ws connection => no_hook
        (dict(is_offline=False, ws_connected=False, hook_supports_receipts=True), INBOUND_NO_HOOK),
        # connected but no receipt capability => legacy_unverified (NOT no_hook)
        (
            dict(is_offline=False, ws_connected=True, hook_supports_receipts=False),
            INBOUND_LEGACY_UNVERIFIED,
        ),
        # failure newer than success => degraded
        (
            dict(
                is_offline=False,
                ws_connected=True,
                hook_supports_receipts=True,
                last_success_at="2026-05-30T10:00:00+00:00",
                last_failure_at="2026-05-30T11:00:00+00:00",
            ),
            INBOUND_DEGRADED,
        ),
        # success newer than failure => online
        (
            dict(
                is_offline=False,
                ws_connected=True,
                hook_supports_receipts=True,
                last_success_at="2026-05-30T12:00:00+00:00",
                last_failure_at="2026-05-30T11:00:00+00:00",
            ),
            INBOUND_ONLINE,
        ),
        # failure with no prior success => degraded
        (
            dict(
                is_offline=False,
                ws_connected=True,
                hook_supports_receipts=True,
                last_failure_at="2026-05-30T11:00:00+00:00",
            ),
            INBOUND_DEGRADED,
        ),
        # clean connected receipt-capable => online
        (dict(is_offline=False, ws_connected=True, hook_supports_receipts=True), INBOUND_ONLINE),
    ],
)
def test_precedence(kwargs, expected):
    assert compute_inbound_status(**kwargs) == expected
