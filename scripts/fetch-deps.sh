#!/usr/bin/env bash
# Downloads the Firecracker VMM and a guest kernel into the data directory.
set -euo pipefail

DATA="${MINI_SPRITES_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/mini-sprites}"
FC_VERSION="${FC_VERSION:-v1.17.0}"
# Guest kernels are published with Firecracker's CI artifacts, not its releases.
KERNEL_URL="${KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155}"
ARCH=$(uname -m)

mkdir -p "$DATA/bin" "$DATA/kernel"
if [ ! -x "$DATA/bin/firecracker" ]; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-$ARCH.tgz" | tar -xz -C "$tmp"
  install -m 0755 "$tmp/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH" "$DATA/bin/firecracker"
fi
[ -f "$DATA/kernel/vmlinux" ] || curl -fsSL -o "$DATA/kernel/vmlinux" "$KERNEL_URL"
"$DATA/bin/firecracker" --version | head -1
ls -lh "$DATA/kernel/vmlinux"
