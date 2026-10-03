#!/usr/bin/env bash
# Builds a sprite disk image from images/<variant>/Containerfile into
# <data>/images/<variant>.ext4. No root needed: the ext4 image is populated
# inside podman's user namespace so file ownership inside the guest comes out right.
#
#   ./scripts/build-image.sh        # base.ext4, the disk every sprite starts from
#   ./scripts/build-image.sh e2b    # e2b.ext4, E2B's userland with envd (docs/images.md)
#   ./scripts/build-image.sh modal  # modal.ext4, Modal's debian_slim (frontend/modal)
set -euo pipefail

cd "$(dirname "$0")/.."
DATA="${WISP_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/wisp}"
DISK_GB="${SPRITE_DISK_GB:-20}"
VARIANT="${1:-base}"
case "$VARIANT" in
  base) ;;
  e2b) ./scripts/build-envd.sh ;; # the image COPYs the binary from its build context
  modal) ;;
  *) echo "usage: build-image.sh [base|e2b|modal]" >&2; exit 2 ;;
esac
TAG="wisp-$VARIANT"
OUT="$DATA/images/$VARIANT.ext4"

mkdir -p "$DATA/images"
podman build -t "$TAG" "images/$VARIANT"

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
