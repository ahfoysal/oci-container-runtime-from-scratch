#!/usr/bin/env bash
# Fetch a minimal busybox rootfs suitable for testing `myrun run`.
#
# Strategy: pull the official busybox image via `docker create` + `docker export`.
# This works on any machine (Linux VM or macOS with Docker Desktop) that has
# Docker available. The resulting ./rootfs directory is self-contained and
# includes /proc (empty, to be mounted by myrun) plus /bin/sh, coreutils, etc.
#
# Usage:
#   ./scripts/fetch-busybox-rootfs.sh [dest-dir]
#
# Default dest-dir: ./rootfs
set -euo pipefail

DEST="${1:-rootfs}"
IMAGE="busybox:latest"

if [[ -d "$DEST" ]]; then
  echo "destination '$DEST' already exists — remove it first or pick another path" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  cat >&2 <<'EOF'
docker not found. Alternatives:

  1) On an Ubuntu VM, install docker.io and rerun this script.
  2) Manually download a busybox rootfs tarball, e.g.:
       curl -L -o busybox.tar.gz \
         https://github.com/docker-library/busybox/raw/dist-amd64/stable/glibc/busybox.tar.gz
       mkdir rootfs && tar -xzf busybox.tar.gz -C rootfs
       mkdir -p rootfs/proc rootfs/sys rootfs/dev
EOF
  exit 1
fi

echo ">>> pulling $IMAGE"
docker pull "$IMAGE"

echo ">>> creating throwaway container"
CID=$(docker create "$IMAGE")

cleanup() { docker rm "$CID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

mkdir -p "$DEST"
echo ">>> exporting rootfs to $DEST"
docker export "$CID" | tar -xf - -C "$DEST"

# Ensure kernel-virtual-fs mount points exist.
mkdir -p "$DEST/proc" "$DEST/sys" "$DEST/dev"

echo ">>> done. rootfs at: $DEST"
echo "    test with: sudo ./myrun run $DEST /bin/sh"
