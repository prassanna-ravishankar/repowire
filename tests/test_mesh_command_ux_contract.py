from __future__ import annotations

from pathlib import Path

from repowire.mcp.server import create_mcp_server

ROOT = Path(__file__).resolve().parents[1]
CONTRACT = ROOT / "docs" / "concepts" / "mesh-command-ux.md"
WEB_CONCEPTS = ROOT / "web" / "app" / "docs" / "concepts" / "page.tsx"


def _contract_text() -> str:
    return CONTRACT.read_text()


def _one_line(text: str) -> str:
    return " ".join(text.split())


def test_mesh_command_contract_names_minimum_command_set() -> None:
    text = _contract_text()

    for command in (
        "status",
        "peers",
        "pending-asks",
        "ask",
        "notify",
        "schedule",
        "timeline",
        "result",
        "doctor",
    ):
        assert f"| `{command}` |" in text

    assert "Optional review controls" in text


def test_mesh_command_contract_defines_json_and_human_rendering() -> None:
    text = _contract_text()

    for field in (
        '"command"',
        '"status"',
        '"schema_version"',
        '"target"',
        '"data"',
        '"warnings"',
        '"next_actions"',
    ):
        assert field in text

    for status in ("`ok`", "`empty`", "`partial`", "`error`"):
        assert status in text

    for section_line in (
        "- `status`: one daemon line",
        "- `peers`: table columns",
        "- `pending-asks`: group inbound and outbound",
        "- `ask`: print the new `correlation_id`",
        "- `notify`: print the notification id only as observability",
        "- `schedule`: print schedule id",
        "- `timeline`: show newest relevant turns/events first",
        "- `result`: show the latest known answer",
        "- `doctor`: keep the existing ok/warn/fail/skip hierarchy",
    ):
        assert section_line in text


def test_mesh_command_contract_preserves_agent_messaging_lifecycle() -> None:
    text = _contract_text()

    required_rules = (
        "Agents must not use `SendMessage` for Repowire peers.",
        "`ask` is non-blocking and returns a `correlation_id` immediately.",
        "bare `ack(correlation_id)`",
        "`ack(correlation_id, message)`",
        "`ask(reply_to=correlation_id, ...)`",
        "`notify` is fire-and-forget.",
        "`schedule --kind ask` opens an ask thread",
        "`schedule --kind notify` delivers a fire-and-forget notification",
    )
    for rule in required_rules:
        assert rule in text


def test_mesh_command_contract_keeps_related_work_out_of_scope() -> None:
    text = _contract_text()
    one_line = _one_line(text)

    assert "not a new daemon job store" in one_line
    assert "must not imply a daemon-backed job lifecycle" in text
    assert "durable tracked work" in text
    assert "ACP/channel broker readiness" in text
    assert "should not claim those states are implemented" in one_line
    assert "a plugin is a convenience package, not a replacement for setup" in one_line


def test_web_docs_mirror_core_command_contract() -> None:
    text = WEB_CONCEPTS.read_text()

    for token in (
        "status",
        "peers",
        "pending-asks",
        "ask",
        "notify",
        "schedule",
        "timeline",
        "result",
        "doctor",
        "schema_version",
        "SendMessage",
        "tracked-work lifecycle",
        "ACP/channel broker health",
    ):
        assert token in text


def test_mcp_tool_instructions_match_command_contract() -> None:
    tools = create_mcp_server()._tool_manager._tools

    list_doc = tools["list_peers"].fn.__doc__ or ""
    ask_doc = tools["ask"].fn.__doc__ or ""
    ack_doc = tools["ack"].fn.__doc__ or ""
    notify_doc = tools["notify_peer"].fn.__doc__ or ""
    broadcast_doc = tools["broadcast"].fn.__doc__ or ""

    assert "Use ask/notify_peer for repowire peers" in _one_line(list_doc)
    assert "SendMessage is only for same-session" in _one_line(list_doc)
    assert "Open a non-blocking ask thread" in ask_doc
    ask_doc_line = _one_line(ask_doc)
    assert "not a synchronous wait or delivery receipt" in ask_doc_line
    assert "Use notify_peer for fire-and-forget" in ask_doc_line
    assert "Do not use SendMessage for mesh peers" in ask_doc
    assert "Bare ack" in ack_doc
    assert "Reply ack" in ack_doc
    assert "Fire-and-forget" in notify_doc
    assert "does NOT guarantee agent receipt" in notify_doc
    assert "Do not use SendMessage" in notify_doc
    assert "Do not use SendMessage" in broadcast_doc
