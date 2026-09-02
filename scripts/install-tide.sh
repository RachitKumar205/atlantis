#!/bin/sh
# Installs tide, the Atlantis CLI.
#
#   curl -fsSL https://releases.tryatlantis.dev/install.sh | sh
#
# Authored in the atlantis repository at scripts/install-tide.sh and uploaded
# beside the tarballs by the release workflow; edit it here.
#
# POSIX sh. Detects OS and architecture, downloads the release tarball from
# the releases host, verifies its checksum against checksums.txt from the
# same release, and installs into the first writable of /usr/local/bin and
# ~/.local/bin.
set -eu

BASE="${TIDE_RELEASES_URL:-https://releases.tryatlantis.dev}"
VERSION="${TIDE_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
    x86_64)  arch=amd64 ;;
    aarch64) arch=arm64 ;;
    arm64)   arch=arm64 ;;
    *) echo "install-tide: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
    darwin|linux) : ;;
    *) echo "install-tide: unsupported platform: $os (Windows installs from the release zip)" >&2; exit 1 ;;
esac

# The latest version is one line at a fixed address, written by the release
# workflow after the tarballs it names are all in place.
if [ "$VERSION" = "latest" ]; then
    VERSION=$(curl -fsSL "$BASE/latest.txt" | head -1 | tr -d '[:space:]')
    [ -n "$VERSION" ] || { echo "install-tide: could not resolve the latest release" >&2; exit 1; }
fi

name="tide-$VERSION-$os-$arch"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "install-tide: downloading $name"
curl -fsSL -o "$tmp/$name.tar.gz" "$BASE/$VERSION/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$BASE/$VERSION/checksums.txt"

# The checksum file travels beside the tarball, so this proves integrity, not
# provenance; the cosign signature on checksums.txt is the provenance check,
# for those who verify it.
(cd "$tmp" && grep " $name.tar.gz\$" checksums.txt | shasum -a 256 -c - >/dev/null) || {
    echo "install-tide: checksum mismatch for $name.tar.gz" >&2; exit 1
}

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

dest=""
for d in /usr/local/bin "$HOME/.local/bin"; do
    [ -d "$d" ] || mkdir -p "$d" 2>/dev/null || continue
    if [ -w "$d" ]; then dest="$d"; break; fi
done
[ -n "$dest" ] || { echo "install-tide: no writable install directory; copy $tmp/$name/tide yourself" >&2; exit 1; }

install -m 0755 "$tmp/$name/tide" "$dest/tide"
echo "install-tide: installed $("$dest/tide" version 2>/dev/null | head -1 || echo tide) to $dest/tide"

case ":$PATH:" in
    *":$dest:"*) : ;;
    *) echo "install-tide: add $dest to your PATH" ;;
esac

echo
echo "Next: run \`tide login\` from your project."
