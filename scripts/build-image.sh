#!/usr/bin/env bash
# Builds the base sprite disk image from images/base/Containerfile. No root
# needed: the ext4 image is populated inside podman's user namespace so file
# ownership inside the guest comes out right.
set -euo pipefail

cd "$(dirname "$0")/.."
DATA="${WISP_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/wisp}"
DISK_GB="${SPRITE_DISK_GB:-20}"
TAG=wisp-base
OUT="$DATA/images/base.ext4"

mkdir -p "$DATA/images"
podman build -t "$TAG" images/base

rm -f "$OUT.tmp"
truncate -s "${DISK_GB}G" "$OUT.tmp"
podman unshare bash -euo pipefail -c '
  mnt=$(podman image mount "$1")
  trap "podman image umount \"$1\" >/dev/null" EXIT
  /usr/sbin/mkfs.ext4 -q -F -L sprite -d "$mnt" \
    -E lazy_itable_init=1,lazy_journal_init=1 "$2"
' _ "$TAG" "$OUT.tmp"
mv "$OUT.tmp" "$OUT"
echo "built $OUT ($(du -h "$OUT" | cut -f1) on disk, ${DISK_GB}G apparent)"
