from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CONTRACT = ROOT / "docs" / "concepts" / "tracked-work-lifecycle.md"
CONCEPT_INDEX = ROOT / "docs" / "concepts" / "index.md"
MESH_COMMAND_CONTRACT = ROOT / "docs" / "concepts" / "mesh-command-ux.md"


def _contract_text() -> str:
    return CONTRACT.read_text()


def _one_line(text: str) -> str:
    return " ".join(text.split())


def test_tracked_work_contract_defines_canonical_states() -> None:
    text = _contract_text()

    for state in (
        "queued",
        "delivered",
        "running",
        "awaiting_input",
        "completed",
        "failed",
        "cancelled",
        "blocked",
        "expired",
        "unavailable",
    ):
        assert f"| `{state}` |" in text

    for terminal_state in ("completed", "failed", "cancelled", "expired", "unavailable"):
        assert re.search(rf"\| `{terminal_state}` \|.*\| Yes \|", text)

    for non_terminal_state in (
        "queued",
        "delivered",
        "running",
        "awaiting_input",
        "blocked",
    ):
        assert re.search(rf"\| `{non_terminal_state}` \|.*\| No \|", text)


def test_tracked_work_contract_separates_ask_ack_from_work_lifecycle() -> None:
    text = _contract_text()
    one_line = _one_line(text)

    assert "separately from conversational `ask`/`ack`" in one_line
    assert "`ask` opens a non-blocking conversational thread" in text
    assert "Tracked work opens a daemon work record and returns a `work_id`" in text
    assert "acking the ask does not complete the work" in one_line
    assert (
        "Ask reminder, pending reply, TTL, and reply-routing behavior remain owned by `AskTracker`"
        in one_line
    )


def test_tracked_work_contract_pins_status_result_and_cancel_semantics() -> None:
    text = _contract_text()

    for field in (
        "`work_id`",
        "`state`",
        "`state_reason`",
        "`phase`",
        "`progress`",
        "`owner_peer_id`",
        "`repowire_session_id`",
        "`circle`",
        "`result_summary`",
    ):
        assert field in text

    for phrase in (
        "`result` is available only for terminal work",
        "Non-terminal result reads should return the current `status`",
        "`cancel(work_id)` records a cancel request",
        "Terminal states win over late cancel requests",
        "Cancel requests must be audit-visible",
    ):
        assert phrase in _one_line(text)


def test_tracked_work_contract_requires_cancel_before_transport_teardown() -> None:
    text = _contract_text()

    assert "protocol-level cancel must be attempted before closing the transport" in text
    assert "Mark the work with `state_reason=cancel_requested`" in text
    assert "Send the backend/runtime protocol cancel request" in text
    assert "Wait only for the configured bounded acknowledgement window" in text
    assert (
        "This contract defines ordering only; it does not expand ACP/channel health diagnostics"
        in _one_line(text)
    )


def test_tracked_work_contract_defines_storage_and_visibility_boundaries() -> None:
    text = _contract_text()
    one_line = _one_line(text)

    assert "Tracked work is daemon control state" in text
    assert "raw runtime transcript bodies" in text
    assert "Beads ledgers or product-repo issue tracker data" in text
    assert "Retention cleanup must follow Repowire's lazy-repair philosophy" in one_line
    assert "`repowire_session_id` groups work with a durable workstream" in text
    assert "`circle` scopes default visibility and name resolution" in text
    assert "Exact IDs override display names" in text
    assert "must not silently fall back to a peer with the same display name" in one_line


def test_tracked_work_contract_keeps_related_work_out_of_scope() -> None:
    text = _contract_text()

    for phrase in (
        "No changes to ask reminder",
        "No ACP/channel broker health matrix",
        "No Claude plugin packaging",
        "No SQLite cleanup",
        "No dashboard UI implementation",
        "No graphify update requirement",
        "No automatic Beads issue import/export",
    ):
        assert phrase in text


def test_concept_docs_link_tracked_work_contract() -> None:
    assert "[Tracked work lifecycle](tracked-work-lifecycle.md)" in CONCEPT_INDEX.read_text()
    assert (
        "[tracked-work lifecycle](tracked-work-lifecycle.md)"
        in MESH_COMMAND_CONTRACT.read_text()
    )
