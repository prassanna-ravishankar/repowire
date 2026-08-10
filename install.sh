#!/bin/sh
# Repowire native installer — curl -fsSL https://github.com/prassanna-ravishankar/repowire/releases/latest/download/install.sh | sh
set -eu

repo="prassanna-ravishankar/repowire"
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "Error: Repowire supports macOS and Linux (got $os)." >&2; exit 1 ;;
esac
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Error: unsupported architecture $arch." >&2; exit 1 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "Error: curl is required." >&2
  exit 1
fi

version=${REPOWIRE_VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/$repo/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n 1)
fi
if [ -z "$version" ]; then
  echo "Error: could not determine the latest Repowire release." >&2
  exit 1
fi
case "$version" in v*) ;; *) version="v$version" ;; esac

asset="repowire_${version#v}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/repowire.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

echo "Installing Repowire $version for $os/$arch..."
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
expected=$(awk -v name="$asset" '$2 == name {print $1}' "$tmp/checksums.txt")
if [ -z "$expected" ]; then
  echo "Error: release checksum for $asset is missing." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
if [ "$actual" != "$expected" ]; then
  echo "Error: checksum mismatch for $asset." >&2
  exit 1
fi

root=${REPOWIRE_INSTALL_DIR:-"$HOME/.local/share/repowire"}
bin_dir=${REPOWIRE_BIN_DIR:-"$HOME/.local/bin"}
target="$root/$version"
mkdir -p "$target" "$bin_dir"
tar -xzf "$tmp/$asset" -C "$target"
chmod +x "$target/repowire"
ln -sfn "$target/repowire" "$bin_dir/repowire"

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "Note: add $bin_dir to PATH." ;;
esac
if ! command -v tmux >/dev/null 2>&1; then
  echo "Warning: tmux is not installed (needed for peer spawning)."
fi

echo "Installed Repowire $version to $target."
"$target/repowire" setup "$@"
