#!/usr/bin/env bash
# Builds E2B's in-guest daemon, envd, from a pinned commit of github.com/e2b-dev/infra
# (packages/envd, Apache-2.0) into a static linux/amd64 binary for the E2B guest image
# (images/e2b). The source is cloned into a cache outside the repo; the binary lands
# at images/e2b/envd, which is git-ignored. See docs/images.md#the-e2b-image.
#
#   ./scripts/build-envd.sh            # builds unless images/e2b/envd is already this pin
#   ./scripts/build-envd.sh --force    # rebuild anyway
#
# To move the pin: change ENVD_COMMIT and ENVD_VERSION together (the script refuses a
# binary that reports another version), and note the new pair in docs/providers/e2b.md
# section 7. The version is what an E2B front-end reports to the SDKs as envdVersion,
# and the SDKs gate features on it (0.6.4 unlocks all of them as of SDK 2.52.0).
set -euo pipefail

ENVD_REPO=https://github.com/e2b-dev/infra.git
ENVD_COMMIT=92197909dce5a1bef33e764ae4af76f0732fd7a8 # 2026-10-03, main
ENVD_VERSION=0.9.0                                   # packages/envd/pkg/version.go at that commit

cd "$(dirname "$0")/.."
OUT=images/e2b/envd
SRC="${WISP_ENVD_SRC:-${XDG_CACHE_HOME:-$HOME/.cache}/wisp/e2b-infra}"

if [ "${1:-}" != "--force" ] && [ -x "$OUT" ] && [ "$("$OUT" -commit 2>/dev/null)" = "${ENVD_COMMIT:0:7}" ]; then
  echo "$OUT is already envd $ENVD_VERSION (${ENVD_COMMIT:0:7})"
  exit 0
fi

if [ ! -d "$SRC/.git" ]; then
  mkdir -p "$(dirname "$SRC")"
  git clone --filter=blob:none --no-checkout "$ENVD_REPO" "$SRC"
fi
if ! git -C "$SRC" cat-file -e "$ENVD_COMMIT^{commit}" 2>/dev/null; then
  git -C "$SRC" fetch --filter=blob:none origin "$ENVD_COMMIT"
fi
git -C "$SRC" -c advice.detachedHead=false checkout -q --force "$ENVD_COMMIT"

# The license is what makes shipping this binary in our image fine; check it is still the one we read.
if ! head -3 "$SRC/LICENSE" | grep -q 'Apache License' || ! head -3 "$SRC/LICENSE" | grep -q 'Version 2.0'; then
  echo "e2b-dev/infra at $ENVD_COMMIT is no longer Apache-2.0 licensed; not building" >&2
  exit 1
fi

# Static: the guest image has a libc, but the binary must not depend on which one.
# -X main.commitSHA is how upstream's Makefile stamps provenance (envd -commit).
mkdir -p "$(dirname "$OUT")"
(cd "$SRC/packages/envd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -buildid= -X=main.commitSHA=${ENVD_COMMIT:0:7}" -o "$OLDPWD/$OUT.tmp" .)

got=$("$OUT.tmp" -version)
if [ "$got" != "$ENVD_VERSION" ]; then
  rm -f "$OUT.tmp"
  echo "envd at $ENVD_COMMIT reports version $got, expected $ENVD_VERSION; update ENVD_VERSION" >&2
  exit 1
fi
mv "$OUT.tmp" "$OUT"
echo "built $OUT: envd $got (${ENVD_COMMIT:0:7}), $(du -h "$OUT" | cut -f1)"
