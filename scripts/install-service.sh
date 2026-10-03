#!/usr/bin/env bash
# Runs wispd (or sandboxd) as a systemd *user* service, so the API comes back
# after a reboot the way the network and the storage volume already do. No root:
# the daemon runs as you, exactly as `make run` does.
#
#   make install-service                         # build, install, (re)start
#   ./scripts/install-service.sh -- --url-domain sprites.example.com --max-running 8
#   ./scripts/install-service.sh --uninstall
#   make install-sandboxd NAME=sandboxd DATA=~/.local/share/sandboxd FLAGS='--listen ...'
#
# Flags after `--` are the daemon's and are remembered in
# ~/.config/wisp/<name>.env; without them a re-install keeps what is there.
#
#   --name NAME   unit name, default wisp (a second stack needs its own)
#   --data DIR    data directory, default $WISP_DATA or ~/.local/share/wisp
#   --bin BIN     wispd (default) or sandboxd. sandboxd needs an explicit --name
#                 other than wisp, an explicit --data other than wisp's, and --listen
#   --check       only run the checks below and exit (make install-sandboxd runs
#                 it before building anything into --data)
#   --force-pair  allow a name other than wisp on wisp's data directory, or the
#                 name wisp on another one (refused by default: either is
#                 usually a typo that would repoint or fight the main install)
#
# An existing <name>.service whose binary or data directory differs from what
# is being installed is never repointed: uninstall it first.
#
# Two things need root, once, and this script only tells you about them:
#
#   sudo loginctl enable-linger $USER
#       Without lingering your user manager, and wispd in it, starts at your
#       first login and stops at your last logout.
#   sudo ./scripts/install-service.sh --system-dropin
#       Installs /etc/systemd/system/user@<uid>.service.d/wisp.conf, which
#       (a) orders your user manager after the sprite network and volume units at
#       boot, and so before them at shutdown, and (b) lifts its stop timeout.
#       Ubuntu ships that at 5 seconds: at reboot everything you run is killed 5 s
#       after being asked to stop, which is not enough to write several guests'
#       RAM to disk. Sprites cut off that way are intact but come back cold.
set -euo pipefail
trap 'echo "install-service.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

NAME=wisp
DATA="${WISP_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/wisp}"
BIN=wispd
NAME_SET=0
DATA_SET=0
FORCE_PAIR=0
ACTION=install
FLAGS=()
HAVE_FLAGS=0
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; NAME_SET=1; shift 2 ;;
    --data) DATA="$2"; DATA_SET=1; shift 2 ;;
    --bin) BIN="$2"; shift 2 ;;
    --force-pair) FORCE_PAIR=1; shift ;;
    --check) ACTION=check; shift ;;
    --uninstall) ACTION=uninstall; shift ;;
    --system-dropin) ACTION=dropin; shift ;;
    --remove-system-dropin) ACTION=rmdropin; shift ;;
    --) shift; FLAGS=("$@"); HAVE_FLAGS=1; break ;;
    *) sed -n '2,38p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
done

# Generous on purpose: a stop is N parallel snapshot writes of guest RAM each.
STOP_TIMEOUT=600

if [ "$ACTION" = dropin ] || [ "$ACTION" = rmdropin ]; then
  [ "$(id -u)" = 0 ] || { echo "run this one with sudo" >&2; exit 1; }
  OWNER="${WISP_OWNER:-${SUDO_USER:-}}"
  [ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine the owning user; run via sudo from your account" >&2; exit 1; }
  DIR="/etc/systemd/system/user@$(id -u "$OWNER").service.d"
  if [ "$ACTION" = rmdropin ]; then
    rm -f "$DIR/wisp.conf"; rmdir "$DIR" 2>/dev/null || true
    systemctl daemon-reload
    echo "ok: removed $DIR/wisp.conf"
    exit 0
  fi
  mkdir -p "$DIR"
  # Every network pool's boot units, since this manager may run a wispd on any of
  # them (--net-pool); pool 0's are named even before setup-host.sh has made them.
  AFTER="wisp-net.service wisp-netd.service"
  for u in /etc/systemd/system/wisp-net[0-9]*.service /etc/systemd/system/wisp-netd[0-9]*.service; do
    [ -e "$u" ] && AFTER+=" $(basename "$u")"
  done
  cat > "$DIR/wisp.conf" <<EOF
# Installed by wisp scripts/install-service.sh --system-dropin.
# $OWNER's user manager runs wispd. Start it after the sprite network and
# volume exist; stop it before they go; and let it finish suspending sprites
# (the distribution default gives user services 5 seconds at shutdown).
[Unit]
After=$AFTER wisp-storage.service

[Service]
TimeoutStopSec=$((STOP_TIMEOUT + 30))
EOF
  systemctl daemon-reload
  echo "ok: $DIR/wisp.conf installed (ordering + a $((STOP_TIMEOUT + 30)) s stop timeout for $OWNER's user manager); takes effect the next time that manager starts, i.e. at the next boot"
  exit 0
fi

[ "$(id -u)" != 0 ] || { echo "run this as yourself, not root: it installs a user service" >&2; exit 1; }
# Before anything, uninstall included: an empty or path-like name would make
# LIB below a parent of every stack's directory, and uninstall rm -rf's LIB.
case "$NAME" in
  ''|*/*|.*) echo "install-service.sh: refusing: bad --name '$NAME'" >&2; exit 1 ;;
esac
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT="$UNIT_DIR/$NAME.service"
ENV_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/wisp/$NAME.env"
LIB="$HOME/.local/lib/wisp/$NAME"

if [ "$ACTION" = uninstall ]; then
  # Stopping suspends every running sprite, so they are warm for whatever runs next.
  systemctl --user disable --now "$NAME.service" 2>/dev/null || true
  rm -f "$UNIT"; rm -rf "$LIB"
  systemctl --user daemon-reload
  echo "ok: $NAME.service removed (kept: $ENV_FILE, and all data in $DATA)"
  exit 0
fi

refuse() { echo "install-service.sh: refusing: $*" >&2; exit 1; }

# Guard rails, all checked before anything is written: the main install (name
# wisp, wispd, wisp's data directory) must only ever be reinstalled as itself.
case "$BIN" in
  wispd|sandboxd) ;;
  *) refuse "--bin must be wispd or sandboxd, not '$BIN'" ;;
esac
DATA="$(realpath -m "$DATA")"
# wisp's data directory, under both spellings the tooling uses (the scripts
# honour XDG_DATA_HOME, the Makefile does not).
IS_PROD_DATA=0
for d in "${XDG_DATA_HOME:-$HOME/.local/share}/wisp" "$HOME/.local/share/wisp"; do
  [ "$DATA" = "$(realpath -m "$d")" ] && IS_PROD_DATA=1
done
if [ "$BIN" = sandboxd ]; then
  [ "$NAME_SET" = 1 ] && [ "$NAME" != wisp ] \
    || refuse "--bin sandboxd needs an explicit --name other than wisp (wisp.service is the main wispd)"
  [ "$DATA_SET" = 1 ] && [ "$IS_PROD_DATA" = 0 ] \
    || refuse "--bin sandboxd needs an explicit --data other than wisp's data directory ($DATA is not allowed or was not given)"
fi
if [ "$FORCE_PAIR" = 0 ]; then
  [ "$NAME" = wisp ] || [ "$IS_PROD_DATA" = 0 ] \
    || refuse "--name $NAME on wisp's data directory $DATA would run a second daemon on the main install's sprites; pass --data, or --force-pair if you mean it"
  [ "$NAME" != wisp ] || [ "$IS_PROD_DATA" = 1 ] \
    || refuse "--name wisp is the main install, but --data is $DATA; pass another --name, or --force-pair if you mean to move it"
fi
FLAGS_VAR=WISPD_FLAGS
[ "$BIN" = wispd ] || FLAGS_VAR=SANDBOXD_FLAGS
# Never silently repoint an existing service at another binary or data directory.
if [ -e "$UNIT" ]; then
  EXEC=$(sed -n 's/^ExecStart=//p' "$UNIT" | head -n1)
  read -r OLD_BIN _ <<<"$EXEC" || true
  OLD_DATA=$(grep -oE -- '--data [^ ]+' <<<"$EXEC" | head -n1 | cut -d' ' -f2 || true)
  if [ "$OLD_BIN" != "$LIB/$BIN" ] || [ "$OLD_DATA" != "$DATA" ]; then
    refuse "$UNIT exists and runs '${OLD_BIN:-?}' on '${OLD_DATA:-?}', not $LIB/$BIN on $DATA; uninstall it first (--name $NAME --uninstall) if you mean to replace it"
  fi
fi
if [ "$BIN" = sandboxd ]; then
  # sandboxd's listener defaults are other daemons' ports (7788 is wispd's).
  if [ "$HAVE_FLAGS" = 1 ]; then NEW_FLAGS="${FLAGS[*]}"; else NEW_FLAGS="$(sed -n "s/^$FLAGS_VAR=//p" "$ENV_FILE" 2>/dev/null || true)"; fi
  grep -qE -- '(^|[[:space:]])--?listen[= ]' <<<"$NEW_FLAGS" \
    || refuse "sandboxd needs an explicit --listen in its flags (the default, 127.0.0.1:7788, is wispd's); add it after --"
fi
[ "$ACTION" != check ] || { echo "ok: $NAME.service may be installed ($BIN on $DATA)"; exit 0; }

REPO="$(cd "$(dirname "$0")/.." && pwd)"
[ -x "$REPO/bin/$BIN" ] || { echo "no $REPO/bin/$BIN: run make build (or make install-service)" >&2; exit 1; }
[ -x "$REPO/bin/wispd" ] || { echo "no $REPO/bin/wispd: run make build (or make install-service)" >&2; exit 1; }
for need in bin/firecracker kernel/vmlinux images/base.ext4 initrd.cpio; do
  [ -e "$DATA/$need" ] || { echo "missing $DATA/$need: run make deps image initrd first" >&2; exit 1; }
done

# A daemon started by hand on this data directory has to go first; two would
# fight over every sprite. (It suspends its sprites on ^C, and they resume here.)
if ! systemctl --user is-active -q "$NAME.service" \
   && "$REPO/bin/wispd" status --data "$DATA" --json 2>/dev/null | grep -q '"daemon": {'; then
  echo "a daemon that is not $NAME.service is running on $DATA; stop it (^C suspends its sprites) and run this again" >&2
  exit 1
fi

# An env file kept from an install of the other binary under this name defines
# the other variable: the unit would expand an unset one and start the daemon
# on its defaults (wispd's 127.0.0.1:7788, the main install's port).
if [ "$HAVE_FLAGS" != 1 ] && [ -e "$ENV_FILE" ] && ! grep -q "^$FLAGS_VAR=" "$ENV_FILE"; then
  refuse "$ENV_FILE has no $FLAGS_VAR= line (left from another binary's install?); pass flags after -- to rewrite it"
fi
mkdir -p "$UNIT_DIR" "$LIB" "$(dirname "$ENV_FILE")"
if [ "$HAVE_FLAGS" = 1 ] || [ ! -e "$ENV_FILE" ]; then
  { echo "# $BIN flags for $NAME.service; edit, then: systemctl --user restart $NAME"
    echo "$FLAGS_VAR=${FLAGS[*]}"; } > "$ENV_FILE"
fi
CUR_FLAGS="$(sed -n "s/^$FLAGS_VAR=//p" "$ENV_FILE")"

# The unit must not depend on a git checkout staying where it is. wispd always
# goes in too: it is the operator CLI (status, keys, ...) for either daemon.
for b in $(printf '%s\n' wispd "$BIN" | sort -u); do
  install -m 0755 "$REPO/bin/$b" "$LIB/$b.new"
  mv -f "$LIB/$b.new" "$LIB/$b"
done

# A user unit cannot be ordered after system units, so wait for what the boot
# units provide. Starting early would be worse than starting late: without the
# volume mounted wispd would see an empty sprite directory, and without the
# bridge it would cold-boot every sprite with no NIC.
WAIT=""
SYSUNITS="${WISP_SYSTEM_UNIT_DIR:-/etc/systemd/system}" # overridable for scripts/test-install-service.sh
if grep -qs -- " $DATA/vm\$" "$SYSUNITS/wisp-storage.service"; then
  WAIT+="mountpoint -q '$DATA/vm' && "
fi
# A second daemon's data directory on a filesystem of its own (a nofail fstab entry, say) has
# to be mounted first, or the daemon would start on an empty directory on whatever is beneath
# it. Not the main install, which waits on wisp-storage above and keeps rendering as it did.
if [ "$NAME" != wisp ]; then
  DATA_MNT=$(findmnt -no TARGET -T "$DATA" 2>/dev/null || true)
  if [ -n "$DATA_MNT" ] && [ "$DATA_MNT" != / ]; then
    WAIT+="mountpoint -q '$DATA_MNT' && "
  fi
fi
# The network pool (wispd --net-pool) names the bridge and its boot unit: pool 0 is
# msbr0 and wisp-net, pool N msbrN and wisp-netN.
# Go's flag package takes -net-pool as well as --net-pool, and the last one wins.
POOL=$(grep -oE -- '(^|[[:space:]])--?net-pool[= ]+[0-9]+' <<<"$CUR_FLAGS" | tail -n1 | grep -oE '[0-9]+$' || true)
POOL=${POOL:-0}
if [ -e "$SYSUNITS/wisp-net$([ "$POOL" = 0 ] || echo "$POOL").service" ] && ! grep -q -- '--net=false' <<<"$CUR_FLAGS"; then
  WAIT+="[ -e /sys/class/net/msbr$POOL ] && "
fi
cat > "$LIB/wait-host.sh" <<EOF
#!/bin/sh
# Generated by install-service.sh: wait (up to 90 s) for the boot units' work.
for i in \$(seq 90); do
  ${WAIT}exit 0
  sleep 1
done
echo "still waiting for the sprite volume or bridge (wisp-storage / wisp-net units)" >&2
exit 1
EOF
chmod 0755 "$LIB/wait-host.sh"

# Any other stack starts after the main one, so after a reboot wisp's sprites
# come back before a second daemon's cold boots compete with them.
ORDER=""
if [ "$NAME" != wisp ] && [ -e "$UNIT_DIR/wisp.service" ]; then
  ORDER="After=wisp.service
"
fi
cat > "$UNIT" <<EOF
# Generated by wisp scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=wisp API daemon ($BIN, data in $DATA)
${ORDER}
[Service]
Type=exec
EnvironmentFile=$ENV_FILE
ExecStartPre=$LIB/wait-host.sh
ExecStart=$LIB/$BIN --data $DATA \$$FLAGS_VAR
# SIGTERM goes to wispd alone, which suspends every running sprite to disk
# before it exits. The default (signal the whole cgroup) would kill the VMs
# under it first and lose their memory.
KillMode=mixed
TimeoutStopSec=$STOP_TIMEOUT
Restart=on-failure
RestartSec=5
# One guest dying of memory pressure is that sprite's problem, not the service's.
OOMPolicy=continue
ManagedOOMPreference=avoid
# A snapshot restore maps guest RAM from a file, and every VM holds a few fds.
LimitNOFILE=65536

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable -q "$NAME.service"
systemctl --user restart "$NAME.service"
for i in $(seq 100); do
  "$LIB/wispd" status --data "$DATA" --json 2>/dev/null | grep -q '"daemon": {' && break
  systemctl --user is-failed -q "$NAME.service" && break
  sleep 0.1
done
if ! systemctl --user is-active -q "$NAME.service"; then
  echo "$NAME.service did not come up:" >&2
  journalctl --user -u "$NAME.service" -n 15 --no-pager >&2 || true
  exit 1
fi

echo "ok: $NAME.service is running and enabled ($BIN --data $DATA ${CUR_FLAGS})"
echo "    status:  $LIB/wispd status --data $DATA"
echo "    logs:    journalctl --user -u $NAME -f        (add -b for this boot, -p warning for trouble only)"
echo "    flags:   $ENV_FILE, then systemctl --user restart $NAME"
echo "    stop:    systemctl --user stop $NAME          (suspends every running sprite first; they resume warm)"
if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
  echo
  echo "NOT DONE, needs you: lingering is off for $USER, so this service only runs while you are logged in."
  echo "    sudo loginctl enable-linger $USER"
fi
if [ ! -e "/etc/systemd/system/user@$(id -u).service.d/wisp.conf" ]; then
  echo
  echo "NOT DONE, needs root once: boot/shutdown ordering and a stop timeout long enough to suspend sprites at"
  echo "reboot (this distribution kills user services after $(systemctl show "user@$(id -u).service" -p TimeoutStopUSec --value 2>/dev/null || echo '?')). Until then a reboot is safe but sprites may come back cold."
  echo "    sudo $REPO/scripts/install-service.sh --system-dropin"
fi
