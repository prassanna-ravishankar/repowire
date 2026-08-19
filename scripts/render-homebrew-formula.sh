#!/bin/sh
# Render Formula/repowire.rb from checksums produced by publish.yml.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 VERSION CHECKSUMS_FILE" >&2
  exit 2
fi

version=${1#v}
checksums=$2

checksum() {
  asset=$1
  value=$(awk -v asset="$asset" '$2 == asset { print $1; exit }' "$checksums")
  if [ -z "$value" ]; then
    echo "missing checksum for $asset" >&2
    exit 1
  fi
  printf '%s' "$value"
}

darwin_arm64="repowire_${version}_darwin_arm64.tar.gz"
darwin_amd64="repowire_${version}_darwin_amd64.tar.gz"
linux_arm64="repowire_${version}_linux_arm64.tar.gz"
linux_amd64="repowire_${version}_linux_amd64.tar.gz"
base="https://github.com/prassanna-ravishankar/repowire/releases/download/v${version}"
darwin_arm64_sha=$(checksum "$darwin_arm64")
darwin_amd64_sha=$(checksum "$darwin_amd64")
linux_arm64_sha=$(checksum "$linux_arm64")
linux_amd64_sha=$(checksum "$linux_amd64")

cat <<EOF
class Repowire < Formula
  desc "Mesh network for AI coding agents"
  homepage "https://repowire.io"
  version "${version}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "${base}/${darwin_arm64}"
      sha256 "${darwin_arm64_sha}"
    else
      url "${base}/${darwin_amd64}"
      sha256 "${darwin_amd64_sha}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "${base}/${linux_arm64}"
      sha256 "${linux_arm64_sha}"
    else
      url "${base}/${linux_amd64}"
      sha256 "${linux_amd64_sha}"
    end
  end

  def install
    prefix.install "repowire", "web"
    bin.install_symlink prefix/"repowire"
  end

  def caveats
    <<~EOS
      Finish installation and start the local service with:
        repowire setup

      Before removing the formula, detach hooks and stop services with:
        repowire uninstall
    EOS
  end

  test do
    assert_equal version.to_s, shell_output("#{bin}/repowire version").strip
    assert_path_exists prefix/"web/out/dashboard.html"
  end
end
EOF
