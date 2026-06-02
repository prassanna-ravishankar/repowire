#!/usr/bin/env python3
"""Opt-in pre-PR hygiene checklist for Repowire contributors."""

from __future__ import annotations

import argparse
import fnmatch
import subprocess
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

DEFAULT_BASE = "origin/main"
BEADS_LEDGER_PATHS = (".beads/issues.jsonl", "issues.jsonl")


@dataclass(frozen=True)
class Rule:
    name: str
    patterns: tuple[str, ...]
    check: tuple[str, ...]
    reason: str
    graphify: bool = False


RULES: tuple[Rule, ...] = (
    Rule(
        name="CLI and setup surface",
        patterns=(
            "repowire/cli.py",
            "repowire/config/**",
            "repowire/spawn.py",
            "repowire/spawn_hints.py",
            "repowire/service/**",
            "install.sh",
            "repowire.yaml.example",
        ),
        check=(
            "README.md",
            "docs/reference/cli.md",
            "CLAUDE.md",
            "AGENTS.md",
        ),
        reason="commands, flags, config defaults, setup, and spawn output are user-visible.",
    ),
    Rule(
        name="MCP and Python client surface",
        patterns=(
            "repowire/mcp/**",
            "repowire/peer_mcp.py",
            "repowire/client.py",
            "repowire/protocol/**",
            "docs/reference/mcp-tools.md",
            "docs/reference/python-client.md",
        ),
        check=(
            "README.md",
            "docs/reference/mcp-tools.md",
            "docs/reference/python-client.md",
            "CLAUDE.md",
            "AGENTS.md",
        ),
        reason=(
            "tool names, signatures, defaults, return shapes, and routing semantics are public API."
        ),
    ),
    Rule(
        name="Agent runtime and hook behavior",
        patterns=(
            "repowire/hooks/**",
            "repowire/installers/**",
            "repowire/channel/**",
            "repowire/acp/**",
            "repowire/orchestrator/template/AGENTS.md",
        ),
        check=(
            "README.md",
            "docs/use/features/connect-*.md",
            "docs/operate/transports.md",
            "docs/troubleshooting/hooks.md",
            "docs/troubleshooting/channel-auth.md",
            "CLAUDE.md",
            "AGENTS.md",
        ),
        reason=(
            "per-runtime install, hook, channel, and troubleshooting behavior needs to "
            "stay aligned."
        ),
        graphify=True,
    ),
    Rule(
        name="Dashboard and human control surfaces",
        patterns=(
            "web/app/dashboard/**",
            "web/components/**",
            "repowire/telegram/**",
            "repowire/slack/**",
            "repowire/daemon/routes/attachments.py",
            "repowire/relay/**",
            "repowire/daemon/relay_client.py",
        ),
        check=(
            "README.md",
            "docs/use/features/**",
            "docs/operate/relay.md",
            "docs/operate/security.md",
            "images/**",
            "CLAUDE.md",
            "AGENTS.md",
        ),
        reason="dashboard, Telegram, Slack, relay, and attachment behavior are product surfaces.",
    ),
    Rule(
        name="Daemon routing and architecture",
        patterns=(
            "repowire/daemon/**",
            "repowire/session/**",
            "repowire/peer_describe.py",
            "docs/operate/architecture.md",
            "docs/concepts/**",
            "docs/use/workflows/**",
        ),
        check=(
            "README.md",
            "docs/operate/architecture.md",
            "docs/concepts/**",
            "docs/use/workflows/**",
            "CLAUDE.md",
            "AGENTS.md",
        ),
        reason=(
            "routing, lifecycle, scheduling, peer state, and session framing affect "
            "architecture docs."
        ),
        graphify=True,
    ),
)


def run_git(args: list[str]) -> str:
    try:
        return subprocess.check_output(["git", *args], text=True, stderr=subprocess.PIPE)
    except subprocess.CalledProcessError as exc:
        sys.stderr.write(exc.stderr)
        raise SystemExit(exc.returncode) from exc


def merge_base(base: str) -> str:
    return run_git(["merge-base", base, "HEAD"]).strip()


def diff_names(args: list[str]) -> set[str]:
    return set(run_git(["diff", "--name-only", *args]).splitlines())


def changed_files(base: str) -> list[str]:
    base_commit = merge_base(base)
    names = diff_names([f"{base_commit}...HEAD"])
    names.update(diff_names([]))
    names.update(diff_names(["--cached"]))
    names.update(run_git(["ls-files", "--others", "--exclude-standard"]).splitlines())
    return sorted(name for name in names if name)


def beads_ledger_changes(base: str) -> tuple[list[str], list[str]]:
    base_commit = merge_base(base)
    committed = sorted(
        path for path in diff_names([f"{base_commit}...HEAD"]) if path in BEADS_LEDGER_PATHS
    )
    local = diff_names([])
    local.update(diff_names(["--cached"]))
    local.update(run_git(["ls-files", "--others", "--exclude-standard"]).splitlines())
    return committed, sorted(path for path in local if path in BEADS_LEDGER_PATHS)


def backup_and_restore_beads_ledgers(paths: list[str]) -> None:
    if not paths:
        return

    backup_dir = Path(".beads/backup/pre-pr-hygiene")
    backup_dir.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    for path in paths:
        source = Path(path)
        if source.exists():
            backup_path = backup_dir / f"{timestamp}__{path.replace('/', '__')}"
            backup_path.write_bytes(source.read_bytes())

    run_git(["restore", "--staged", "--worktree", "--", *paths])


def matches(path: str, patterns: tuple[str, ...]) -> bool:
    return any(fnmatch.fnmatch(path, pattern) for pattern in patterns)


def touched_docs(files: list[str], patterns: tuple[str, ...]) -> list[str]:
    return [path for path in files if matches(path, patterns)]


def print_rule(rule: Rule, files: list[str]) -> bool:
    hits = [path for path in files if matches(path, rule.patterns)]
    if not hits:
        return False

    docs = touched_docs(files, rule.check)
    print(f"\n## {rule.name}")
    print(f"Reason: {rule.reason}")
    print("Changed paths:")
    for path in hits:
        print(f"  - {path}")

    print("Review these surfaces:")
    for pattern in rule.check:
        marker = "x" if any(matches(path, (pattern,)) for path in docs) else " "
        print(f"  [{marker}] {pattern}")

    if rule.graphify:
        print("Graphify: consider `/graphify . --update` for this architecture-level change.")

    return True


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Print an opt-in pre-PR docs and architecture hygiene checklist."
    )
    parser.add_argument(
        "--base",
        default=DEFAULT_BASE,
        help=f"Base ref to compare against. Defaults to {DEFAULT_BASE}.",
    )
    parser.add_argument(
        "--restore-beads-ledgers",
        action="store_true",
        help=(
            "Restore local Beads JSONL ledger churn after saving a backup under "
            ".beads/backup/pre-pr-hygiene/. Committed ledger changes are only reported."
        ),
    )
    args = parser.parse_args()

    if not Path(".git").exists():
        sys.stderr.write("Run this from the Repowire repo root.\n")
        return 2

    committed_ledgers, local_ledgers = beads_ledger_changes(args.base)
    if args.restore_beads_ledgers and local_ledgers:
        backup_and_restore_beads_ledgers(local_ledgers)
        committed_ledgers, local_ledgers = beads_ledger_changes(args.base)

    files = changed_files(args.base)
    if not files:
        print("No changed files detected.")
        return 0

    print(f"Pre-PR hygiene checklist against {args.base}")
    print("This is advisory; use judgement and explain intentional deferrals in the PR.")

    if committed_ledgers or local_ledgers:
        print("\n## Beads ledger safety")
        print(
            "Mutable Beads JSONL ledgers must not ride along in feature PRs. "
            "They are generated issue state, not product changes."
        )
        if committed_ledgers:
            print("Committed ledger changes found:")
            for path in committed_ledgers:
                print(f"  - {path}")
            print("Rebase or amend these out before opening the PR.")
        if local_ledgers:
            print("Local ledger churn found:")
            for path in local_ledgers:
                print(f"  - {path}")
            print(
                "Run `python3 scripts/pre_pr_hygiene.py --restore-beads-ledgers` "
                "to back up and restore these files."
            )

    print("\nChanged files:")
    for path in files:
        print(f"  - {path}")

    matched = False
    for rule in RULES:
        matched = print_rule(rule, files) or matched

    if not matched:
        print("\nNo product-surface hygiene rules matched these paths.")

    print("\nBefore opening the PR:")
    print("  - Run relevant tests/lint/type checks for the touched area.")
    print("  - If docs are intentionally deferred, file a Beads follow-up and mention it.")
    print("  - Keep generated graphify JSON/cache out of hand-written docs.")

    if committed_ledgers or local_ledgers:
        print("  - Remove Beads JSONL ledger churn before opening the PR.")
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
