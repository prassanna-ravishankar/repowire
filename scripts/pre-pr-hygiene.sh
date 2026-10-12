#!/bin/sh
# Advisory docs/architecture check before opening a PR.
set -eu

base=origin/main
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base=$2; shift 2 ;;
    *) echo "usage: $0 [--base REF]" >&2; exit 2 ;;
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
    echo "Review docs/operate/architecture.md and docs/concepts/." ;;
esac
