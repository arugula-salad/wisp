#!/usr/bin/env bash
# Packs the statically linked wisp-agent as /init in an initramfs, with the
# in-guest sprite-env CLI beside it (the agent installs it onto the disk at boot).
set -euo pipefail

cd "$(dirname "$0")/.."
DATA="${WISP_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/wisp}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/init" ./cmd/wisp-agent
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/sprite-env" ./cmd/sprite-env
mkdir -p "$work/dev" "$work/proc" "$work/newroot" "$DATA"
(cd "$work" && find . -print0 | cpio --null -o -H newc --quiet -R 0:0) > "$DATA/initrd.cpio.tmp"
mv "$DATA/initrd.cpio.tmp" "$DATA/initrd.cpio"
echo "built $DATA/initrd.cpio ($(du -h "$DATA/initrd.cpio" | cut -f1))"
