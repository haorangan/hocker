#!/usr/bin/env sh
# Download and unpack an Alpine mini root filesystem for use as a container image.
# Run this inside the Linux VM where you will run hocker.
#
# Override the version with environment variables if the default link is stale,
# for example: ALPINE_BRANCH=v3.20 ALPINE_VERSION=3.20.3 ./scripts/get-rootfs.sh
set -eu

ALPINE_BRANCH="${ALPINE_BRANCH:-v3.20}"
ALPINE_VERSION="${ALPINE_VERSION:-3.20.3}"
DEST="${1:-rootfs}"

arch="$(uname -m)"
case "$arch" in
	x86_64)        alpine_arch="x86_64" ;;
	aarch64|arm64) alpine_arch="aarch64" ;;
	*) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

tarball="alpine-minirootfs-${ALPINE_VERSION}-${alpine_arch}.tar.gz"
url="https://dl-cdn.alpinelinux.org/alpine/${ALPINE_BRANCH}/releases/${alpine_arch}/${tarball}"

mkdir -p "$DEST"
echo "Downloading ${url}"
if ! curl -fsSL "$url" -o "/tmp/${tarball}"; then
	echo "Download failed. The version may have moved; try setting ALPINE_BRANCH and ALPINE_VERSION." >&2
	exit 1
fi

echo "Unpacking into ${DEST}/"
tar -xzf "/tmp/${tarball}" -C "$DEST"
rm -f "/tmp/${tarball}"

echo "Done. Now run: sudo ./hocker run /bin/sh"
