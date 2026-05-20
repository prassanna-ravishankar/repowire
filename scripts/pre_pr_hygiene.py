#!/usr/bin/env python3
"""Opt-in pre-PR hygiene checklist for Repowire contributors."""

from __future__ import annotations

import argparse
import fnmatch
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

DEFAULT_BASE = "origin/main"


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
            "web/app/docs/reference/cli/page.tsx",
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
            "web/app/docs/reference/tools/page.tsx",
            "web/app/docs/reference/client/page.tsx",
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
            "docs/agents/**",
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
            "web/app/docs/**",
            "web/components/**",
            "repowire/telegram/**",
            "repowire/slack/**",
            "repowire/daemon/routes/attachments.py",
            "repowire/relay/**",
            "repowire/daemon/relay_client.py",
        ),
        check=(
            "README.md",
            "docs/surfaces/**",
            "docs/relay/**",
            "web/app/docs/**",
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
            "docs/reference/architecture.md",
            "docs/concepts/**",
            "docs/patterns/**",
        ),
        check=(
            "README.md",
            "docs/reference/architecture.md",
            "docs/concepts/**",
            "docs/patterns/**",
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


def changed_files(base: str) -> list[str]:
    merge_base = run_git(["merge-base", base, "HEAD"]).strip()
    names = set(run_git(["diff", "--name-only", f"{merge_base}...HEAD"]).splitlines())
    names.update(run_git(["diff", "--name-only"]).splitlines())
    names.update(run_git(["diff", "--name-only", "--cached"]).splitlines())
    names.update(run_git(["ls-files", "--others", "--exclude-standard"]).splitlines())
    return sorted(name for name in names if name)


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
    args = parser.parse_args()

    if not Path(".git").exists():
        sys.stderr.write("Run this from the Repowire repo root.\n")
        return 2

    files = changed_files(args.base)
    if not files:
        print("No changed files detected.")
        return 0

    print(f"Pre-PR hygiene checklist against {args.base}")
    print("This is advisory; use judgement and explain intentional deferrals in the PR.")
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
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
