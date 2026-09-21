#!/usr/bin/env bash
# Optional one-time privileged setup: puts the sprite directory on a reflink-capable
# filesystem, so creating a sprite, taking a checkpoint and restoring one become
# instant copy-on-write clones instead of full disk copies.
#
#   sudo ./scripts/setup-storage.sh            # create + mount + migrate existing sprites
#   sudo ./scripts/setup-storage.sh --remove   # move sprites back to a plain directory
#
# It makes an XFS filesystem (reflink=1) inside one image file, <data>/sprites.xfs,
# loop-mounted at <data>/vm, and installs mini-sprites-storage.service to mount it at
# boot. Nothing outside the data directory is touched. spritesd needs no configuration:
# it probes for reflink support at startup and says which mode it is in.
#
# The image is sparse but is never allowed to promise more than the host can hold:
# a loop filesystem that outgrows its backing disk fails with I/O errors, not ENOSPC.
set -euo pipefail
trap 'echo "setup-storage.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
OWNER="${MINI_SPRITES_OWNER:-${SUDO_USER:-}}"
[ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine owning user; run via sudo from your account" >&2; exit 1; }
OWNER_HOME=$(getent passwd "$OWNER" | cut -d: -f6)
DATA="${MINI_SPRITES_DATA:-$OWNER_HOME/.local/share/mini-sprites}"
IMG="$DATA/sprites.xfs"
MNT="$DATA/vm"
SIZE_GB="${SPRITE_VOLUME_GB:-40}"
UNIT=/etc/systemd/system/mini-sprites-storage.service

mounted() { mountpoint -q "$MNT"; }

require_idle() {
  if pgrep -u "$OWNER" -x spritesd >/dev/null; then
    echo "spritesd is running; stop it first (it suspends its sprites on SIGTERM)" >&2
    exit 1
  fi
  if pgrep -u "$OWNER" -x firecracker >/dev/null; then
    echo "firecracker processes are still running for $OWNER; stop them first" >&2
    exit 1
  fi
}

# kb_used <dir>: real disk usage, which for sparse images is far below their size.
kb_used() { du -sk "$1" 2>/dev/null | cut -f1; }
kb_free() { df -k --output=avail "$1" | tail -1 | tr -d ' '; }

install_unit() {
  cat > "$UNIT" <<EOF
[Unit]
Description=mini-sprites reflink volume ($IMG on $MNT)
RequiresMountsFor=$DATA
ConditionPathExists=$IMG

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/mount -o loop,discard,noatime $IMG $MNT
ExecStop=/usr/bin/umount $MNT

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable mini-sprites-storage.service >/dev/null
}

create() {
  command -v mkfs.xfs >/dev/null || { echo "mkfs.xfs not found: sudo apt install xfsprogs, then run this again" >&2; exit 1; }
  [ -d "$DATA" ] || { echo "no data directory at $DATA (run: make deps image)" >&2; exit 1; }
  require_idle
  if mounted; then
    install_unit
    echo "already set up: $MNT is a mount point ($(findmnt -no FSTYPE,SIZE,USED "$MNT"))"
    exit 0
  fi

  mkdir -p "$MNT"
  local existing free
  existing=$(kb_used "$MNT"); existing=${existing:-0}
  free=$(kb_free "$DATA")
  # During migration the data exists twice. Keep 5 GiB of the host disk for everything else.
  local need=$(( SIZE_GB * 1024 * 1024 + 5 * 1024 * 1024 ))
  if [ "$free" -lt "$need" ]; then
    echo "not enough free space under $DATA for a ${SIZE_GB}G volume plus 5G headroom: $(( free / 1024 / 1024 ))G free." >&2
    echo "choose a smaller one, e.g.: sudo SPRITE_VOLUME_GB=$(( (free / 1024 / 1024 - 5) * 8 / 10 )) $0" >&2
    exit 1
  fi
  if [ "$existing" -gt $(( SIZE_GB * 1024 * 1024 * 8 / 10 )) ]; then
    echo "existing sprites use $(( existing / 1024 / 1024 ))G, too much for a ${SIZE_GB}G volume; set SPRITE_VOLUME_GB higher" >&2
    exit 1
  fi

  rm -f "$IMG.tmp"
  truncate -s "${SIZE_GB}G" "$IMG.tmp"
  mkfs.xfs -q -m reflink=1 -L msprites "$IMG.tmp"
  mv "$IMG.tmp" "$IMG"
  chown "$OWNER": "$IMG"

  local old=""
  if [ -n "$(ls -A "$MNT")" ]; then
    old="$DATA/vm.pre-reflink"
    [ ! -e "$old" ] || { echo "$old already exists from an earlier attempt; inspect and remove it first" >&2; exit 1; }
    mv "$MNT" "$old"
    mkdir "$MNT"
  fi
  mount -o loop,discard,noatime "$IMG" "$MNT"
  chown "$OWNER": "$MNT"
  if [ -n "$old" ]; then
    echo "migrating existing sprites onto the volume..."
    # -a keeps ownership, modes and times; sparse keeps 20G disk images at their real size.
    cp -a --sparse=always "$old"/. "$MNT"/
    rm -rf "$old"
  fi
  install_unit
  echo "ok: $(findmnt -no FSTYPE,SIZE "$MNT") reflink volume mounted at $MNT (image $IMG, mounted at boot by mini-sprites-storage.service)"
  echo "start spritesd; its log should say the sprite volume supports reflinks"
}

remove() {
  require_idle
  if mounted; then
    local used free
    used=$(kb_used "$MNT"); free=$(kb_free "$DATA")
    if [ "$free" -lt $(( used + 5 * 1024 * 1024 )) ]; then
      echo "not enough free space to move $(( used / 1024 / 1024 ))G of sprites back off the volume" >&2
      exit 1
    fi
    local out="$DATA/vm.plain"
    rm -rf "$out"; mkdir "$out"
    # Reflinked clones stop sharing blocks once copied out, so this can grow; checked above
    # against apparent usage, which is the upper bound.
    cp -a --sparse=always "$MNT"/. "$out"/
    umount "$MNT"
    rmdir "$MNT"
    mv "$out" "$MNT"
    chown "$OWNER": "$MNT"
  fi
  if [ -f "$UNIT" ]; then
    # --now runs ExecStop (umount) too; it is already unmounted by then, hence the || true.
    systemctl disable --now mini-sprites-storage.service >/dev/null 2>&1 || true
    rm -f "$UNIT"
    systemctl daemon-reload
  fi
  rm -f "$IMG"
  echo "removed the reflink volume; sprites are back in the plain directory $MNT"
}

case "${1:-}" in
  "") create ;;
  --remove) remove ;;
  *) echo "usage: $0 [--remove]" >&2; exit 2 ;;
esac
