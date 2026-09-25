#!/usr/bin/env bash
# Creates a private wispd data directory that shares the big read-only
# artifacts (firecracker, kernel, base image) with the main one, so several
# isolated stacks can run side by side:
#
#   ./scripts/dev-data.sh /tmp/wisp-a            # keep the path SHORT: it holds unix sockets (108-byte limit)
#   WISP_DATA=/tmp/wisp-a ./scripts/build-initrd.sh
#   ./bin/wispd --data /tmp/wisp-a --listen 127.0.0.1:7801 --net=false
set -euo pipefail
DEST="${1:?usage: dev-data.sh <short-dir>}"
MAIN="${WISP_MAIN_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/wisp}"
mkdir -p "$DEST"
for d in bin kernel images; do
  [ -e "$MAIN/$d" ] || { echo "missing $MAIN/$d (run: make deps image)" >&2; exit 1; }
  ln -sfn "$MAIN/$d" "$DEST/$d"
done
echo "$DEST ready; token will be generated on first start at $DEST/token"
