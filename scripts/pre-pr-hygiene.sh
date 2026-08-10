#!/bin/sh
# Advisory docs/architecture check before opening a PR.
set -eu

base=origin/main
restore=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base=$2; shift 2 ;;
    --restore-beads-ledgers) restore=true; shift ;;
    *) echo "usage: $0 [--base REF] [--restore-beads-ledgers]" >&2; exit 2 ;;
  esac
done

merge_base=$(git merge-base "$base" HEAD)
changes=$(
  {
    git diff --name-only "$merge_base...HEAD"
    git diff --name-only
    git diff --cached --name-only
    git ls-files --others --exclude-standard
  } | sort -u
)

ledger=$(printf '%s\n' "$changes" | grep -E '^(\.beads/issues\.jsonl|issues\.jsonl)$' || true)
if [ -n "$ledger" ]; then
  if [ "$restore" = true ]; then
    backup=".beads/backup/pre-pr-hygiene/$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "$backup"
    for path in $ledger; do
      [ ! -f "$path" ] || cp "$path" "$backup/$(printf '%s' "$path" | tr / _)"
      git restore --staged --worktree -- "$path"
    done
    echo "Restored Beads ledger churn; backup: $backup"
  else
    echo "ERROR: Beads ledger churn is present:"
    printf '  %s\n' $ledger
    echo "Run $0 --restore-beads-ledgers to back it up and restore it."
    exit 1
  fi
fi

echo "Changed files:"
printf '%s\n' "$changes" | sed 's/^/  - /'

case "$changes" in
  *daemon-go/cli/*|*daemon-go/config/*|*install.sh*)
    echo "Review README.md, docs/reference/cli.md, and docs/reference/configuration.md." ;;
esac
case "$changes" in
  *daemon-go/hooks/*|*daemon-go/mcpstdio/*|*daemon-go/hub/routes_mcp*)
    echo "Review docs/operate/transports.md, docs/troubleshooting/hooks.md, and docs/reference/mcp-tools.md." ;;
esac
case "$changes" in
  *daemon-go/relay*|*daemon-go/mobile/*|*web/*)
    echo "Review docs/use/features, docs/operate/relay.md, and docs/operate/security.md." ;;
esac
case "$changes" in
  *daemon-go/peer/*|*daemon-go/service/*|*daemon-go/state/*)
    echo "Review docs/operate/architecture.md and consider refreshing graphify-out/." ;;
esac
