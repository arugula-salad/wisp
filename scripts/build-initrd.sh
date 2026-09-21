#!/usr/bin/env bash
# Packs the statically linked sprite-agent as /init in an initramfs.
set -euo pipefail

cd "$(dirname "$0")/.."
DATA="${MINI_SPRITES_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/mini-sprites}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/init" ./cmd/sprite-agent
mkdir -p "$work/dev" "$work/proc" "$work/newroot" "$DATA"
(cd "$work" && find . -print0 | cpio --null -o -H newc --quiet -R 0:0) > "$DATA/initrd.cpio.tmp"
mv "$DATA/initrd.cpio.tmp" "$DATA/initrd.cpio"
echo "built $DATA/initrd.cpio ($(du -h "$DATA/initrd.cpio" | cut -f1))"
