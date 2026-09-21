#!/usr/bin/env bash
# Optional one-time privileged setup: puts the sprite directory on a reflink-capable
# filesystem, so creating a sprite, taking a checkpoint and restoring one become
# instant copy-on-write clones instead of full disk copies.
#
#   sudo ./scripts/setup-storage.sh            # create + mount + migrate existing sprites
#   sudo ./scripts/setup-storage.sh --remove   # move sprites back to a plain directory
#   sudo SPRITE_VOLUME_GB=300 ./scripts/setup-storage.sh --grow      # make the volume bigger
#   sudo SPRITE_VOLUME_GB=300 SPRITE_VOLUME_IMAGE=/data/mini-sprites/sprites.xfs \
#        ./scripts/setup-storage.sh --grow      # ...and move its image to a roomier disk
#
# It makes an XFS filesystem (reflink=1) inside one image file, <data>/sprites.xfs unless
# SPRITE_VOLUME_IMAGE says otherwise, loop-mounted at <data>/vm, and installs
# mini-sprites-storage.service to mount it at boot. Nothing else is touched. spritesd needs
# no configuration: it probes for reflink support at startup, and finds the image behind
# the mount by itself.
#
# --grow only ever grows (XFS cannot shrink) and keeps every sprite: the filesystem is
# extended in place, and a move copies the image and removes the old one only once the
# copy is mounted.
#
# The image is sparse but is never allowed to promise more than the host can hold:
# a loop filesystem that outgrows its backing disk fails with I/O errors, not ENOSPC.
set -euo pipefail
trap 'echo "setup-storage.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
OWNER="${MINI_SPRITES_OWNER:-${SUDO_USER:-}}"
[ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine owning user; run via sudo from your account" >&2; exit 1; }
OWNER_HOME=$(getent passwd "$OWNER" | cut -d: -f6)
OWNER_GROUP=$(id -gn "$OWNER")
DATA="${MINI_SPRITES_DATA:-$OWNER_HOME/.local/share/mini-sprites}"
MNT="$DATA/vm"
SIZE_GB="${SPRITE_VOLUME_GB:-40}"
UNIT=/etc/systemd/system/mini-sprites-storage.service

mounted() { mountpoint -q "$MNT"; }

# current_image: the file behind the mounted volume, or else the one the boot unit
# names. The image may have been moved off the data directory by --grow.
current_image() {
  local src
  if mounted; then
    src=$(findmnt -no SOURCE "$MNT")
    [ -r "/sys/block/${src##*/}/loop/backing_file" ] && { cat "/sys/block/${src##*/}/loop/backing_file"; return; }
  fi
  [ -f "$UNIT" ] && sed -n 's/^ConditionPathExists=//p' "$UNIT"
}
CUR_IMG="$(current_image || true)"
IMG="${SPRITE_VOLUME_IMAGE:-${CUR_IMG:-$DATA/sprites.xfs}}"

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

# image_dir: make sure the image's directory exists; one created here is the owner's.
image_dir() {
  local d; d=$(dirname "$IMG")
  [ -d "$d" ] || { mkdir -p "$d"; chown "$OWNER:$OWNER_GROUP" "$d"; }
}

# kb_used <dir>: real disk usage, which for sparse images is far below their size.
kb_used() { du -sk "$1" 2>/dev/null | cut -f1; }
kb_free() { df -k --output=avail "$1" | tail -1 | tr -d ' '; }

install_unit() {
  cat > "$UNIT" <<EOF
[Unit]
Description=mini-sprites reflink volume ($IMG on $MNT)
RequiresMountsFor=$DATA $(dirname "$IMG")
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
    IMG="${CUR_IMG:-$IMG}"
    install_unit
    echo "already set up: $MNT is a mount point ($(findmnt -no FSTYPE,SIZE,USED "$MNT"), image $IMG); --grow makes it bigger or moves it"
    exit 0
  fi

  mkdir -p "$MNT"
  local existing free
  existing=$(kb_used "$MNT"); existing=${existing:-0}
  image_dir
  free=$(kb_free "$(dirname "$IMG")")
  # During migration the data exists twice. Keep 5 GiB of the host disk for everything else.
  local need=$(( SIZE_GB * 1024 * 1024 + 5 * 1024 * 1024 ))
  if [ "$free" -lt "$need" ]; then
    echo "not enough free space under $(dirname "$IMG") for a ${SIZE_GB}G volume plus 5G headroom: $(( free / 1024 / 1024 ))G free." >&2
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
  chown "$OWNER:$OWNER_GROUP" "$IMG"

  local old=""
  if [ -n "$(ls -A "$MNT")" ]; then
    old="$DATA/vm.pre-reflink"
    [ ! -e "$old" ] || { echo "$old already exists from an earlier attempt; inspect and remove it first" >&2; exit 1; }
    mv "$MNT" "$old"
    mkdir "$MNT"
  fi
  mount -o loop,discard,noatime "$IMG" "$MNT"
  chown "$OWNER:$OWNER_GROUP" "$MNT"
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

grow() {
  command -v xfs_growfs >/dev/null || { echo "xfs_growfs not found: sudo apt install xfsprogs" >&2; exit 1; }
  mounted && [ -n "$CUR_IMG" ] && [ -f "$CUR_IMG" ] || { echo "no mounted sprite volume at $MNT to grow; run this without --grow to create one" >&2; exit 1; }
  require_idle

  local cur_kb new_kb used_kb free_kb moving=0
  cur_kb=$(( $(stat -c %s "$CUR_IMG") / 1024 ))
  new_kb=$cur_kb
  [ -z "${SPRITE_VOLUME_GB:-}" ] || new_kb=$(( SIZE_GB * 1024 * 1024 ))
  [ "$IMG" = "$CUR_IMG" ] || moving=1
  if [ "$new_kb" -lt "$cur_kb" ]; then
    echo "the volume is $(( cur_kb / 1024 / 1024 ))G and XFS cannot shrink; SPRITE_VOLUME_GB must be at least that" >&2
    exit 1
  fi
  if [ "$moving" = 0 ] && [ "$new_kb" = "$cur_kb" ]; then
    echo "nothing to do: $CUR_IMG is already $(( cur_kb / 1024 / 1024 ))G (set SPRITE_VOLUME_GB and/or SPRITE_VOLUME_IMAGE)" >&2
    exit 1
  fi

  # The image is sparse, but it must never promise more than its disk can hold. What
  # it already occupies counts as room only when it stays on the same disk.
  image_dir
  free_kb=$(kb_free "$(dirname "$IMG")")
  used_kb=$(kb_used "$CUR_IMG"); used_kb=${used_kb:-0}
  [ "$moving" = 1 ] || free_kb=$(( free_kb + used_kb ))
  if [ "$free_kb" -lt $(( new_kb + 5 * 1024 * 1024 )) ]; then
    echo "not enough room under $(dirname "$IMG") for a $(( new_kb / 1024 / 1024 ))G volume plus 5G headroom: $(( free_kb / 1024 / 1024 ))G available." >&2
    exit 1
  fi

  umount "$MNT"
  if [ "$moving" = 1 ]; then
    [ ! -e "$IMG" ] || { echo "$IMG already exists; not overwriting it" >&2; mount -o loop,discard,noatime "$CUR_IMG" "$MNT"; exit 1; }
    echo "copying the volume image ($(( used_kb / 1024 / 1024 ))G in use) to $IMG..."
    rm -f "$IMG.tmp"
    if ! cp --sparse=always "$CUR_IMG" "$IMG.tmp"; then
      rm -f "$IMG.tmp"; mount -o loop,discard,noatime "$CUR_IMG" "$MNT"
      echo "copy failed; the volume is mounted from $CUR_IMG as before" >&2; exit 1
    fi
    mv "$IMG.tmp" "$IMG"
    chown "$OWNER:$OWNER_GROUP" "$IMG"
  fi
  truncate -s "${new_kb}K" "$IMG"
  if ! mount -o loop,discard,noatime "$IMG" "$MNT"; then
    [ "$moving" = 0 ] || mount -o loop,discard,noatime "$CUR_IMG" "$MNT"
    echo "could not mount $IMG; $([ "$moving" = 1 ] && echo "the volume is mounted from $CUR_IMG as before and $IMG is left for inspection" || echo "inspect it with xfs_repair -n")" >&2
    exit 1
  fi
  xfs_growfs "$MNT" >/dev/null
  install_unit
  [ "$moving" = 0 ] || rm -f "$CUR_IMG"
  echo "ok: $(findmnt -no FSTYPE,SIZE,AVAIL "$MNT") at $MNT (image $IMG$([ "$moving" = 1 ] && echo ", moved from $CUR_IMG"))"
  echo "start spritesd again; every sprite is as it was"
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
    chown "$OWNER:$OWNER_GROUP" "$MNT"
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
  --grow) grow ;;
  *) echo "usage: $0 [--remove|--grow]" >&2; exit 2 ;;
esac
