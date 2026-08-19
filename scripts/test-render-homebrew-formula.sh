#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/repowire-homebrew-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cat >"$tmp/checksums.txt" <<'EOF'
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  repowire_0.18.0_darwin_arm64.tar.gz
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  repowire_0.18.0_darwin_amd64.tar.gz
cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  repowire_0.18.0_linux_arm64.tar.gz
dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd  repowire_0.18.0_linux_amd64.tar.gz
EOF

"$root/scripts/render-homebrew-formula.sh" v0.18.0 "$tmp/checksums.txt" >"$tmp/repowire.rb"

grep -Fq 'version "0.18.0"' "$tmp/repowire.rb"
grep -Fq 'releases/download/v0.18.0/repowire_0.18.0_darwin_arm64.tar.gz' "$tmp/repowire.rb"
grep -Fq 'sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' "$tmp/repowire.rb"
grep -Fq 'prefix.install "repowire", "web"' "$tmp/repowire.rb"
grep -Fq 'assert_path_exists prefix/"web/out/dashboard.html"' "$tmp/repowire.rb"

if "$root/scripts/render-homebrew-formula.sh" v0.18.0 /dev/null >/dev/null 2>&1; then
  echo "renderer accepted incomplete checksums" >&2
  exit 1
fi

echo "Homebrew formula renderer: ok"
