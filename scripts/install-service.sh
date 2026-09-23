#!/usr/bin/env bash
# Runs wispd as a systemd *user* service, so the API comes back after a reboot
# the way the network and the storage volume already do. No root: the daemon
# runs as you, exactly as `make run` does.
#
#   make install-service                         # build, install, (re)start
#   ./scripts/install-service.sh -- --url-domain sprites.example.com --max-running 8
#   ./scripts/install-service.sh --uninstall
#
# Flags after `--` are wispd's and are remembered in
# ~/.config/wisp/<name>.env; without them a re-install keeps what is there.
#
#   --name NAME   unit name, default wisp (a second stack needs its own)
#   --data DIR    data directory, default $WISP_DATA or ~/.local/share/wisp
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
ACTION=install
FLAGS=()
HAVE_FLAGS=0
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2 ;;
    --data) DATA="$2"; shift 2 ;;
    --uninstall) ACTION=uninstall; shift ;;
    --system-dropin) ACTION=dropin; shift ;;
    --remove-system-dropin) ACTION=rmdropin; shift ;;
    --) shift; FLAGS=("$@"); HAVE_FLAGS=1; break ;;
    *) sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
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
  cat > "$DIR/wisp.conf" <<EOF
# Installed by wisp scripts/install-service.sh --system-dropin.
# $OWNER's user manager runs wispd. Start it after the sprite network and
# volume exist; stop it before they go; and let it finish suspending sprites
# (the distribution default gives user services 5 seconds at shutdown).
[Unit]
After=wisp-net.service wisp-netd.service wisp-storage.service

[Service]
TimeoutStopSec=$((STOP_TIMEOUT + 30))
EOF
  systemctl daemon-reload
  echo "ok: $DIR/wisp.conf installed (ordering + a $((STOP_TIMEOUT + 30)) s stop timeout for $OWNER's user manager); takes effect the next time that manager starts, i.e. at the next boot"
  exit 0
fi

[ "$(id -u)" != 0 ] || { echo "run this as yourself, not root: it installs a user service" >&2; exit 1; }
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

REPO="$(cd "$(dirname "$0")/.." && pwd)"
[ -x "$REPO/bin/wispd" ] || { echo "no $REPO/bin/wispd: run make build (or make install-service)" >&2; exit 1; }
DATA="$(realpath -m "$DATA")"
for need in bin/firecracker kernel/vmlinux images/base.ext4 initrd.cpio; do
  [ -e "$DATA/$need" ] || { echo "missing $DATA/$need: run make deps image initrd first" >&2; exit 1; }
done

# A daemon started by hand on this data directory has to go first; two would
# fight over every sprite. (It suspends its sprites on ^C, and they resume here.)
if ! systemctl --user is-active -q "$NAME.service" \
   && "$REPO/bin/wispd" status --data "$DATA" --json 2>/dev/null | grep -q '"daemon": {'; then
  echo "a wispd that is not $NAME.service is running on $DATA; stop it (^C suspends its sprites) and run this again" >&2
  exit 1
fi

mkdir -p "$UNIT_DIR" "$LIB" "$(dirname "$ENV_FILE")"
if [ "$HAVE_FLAGS" = 1 ] || [ ! -e "$ENV_FILE" ]; then
  { echo "# wispd flags for $NAME.service; edit, then: systemctl --user restart $NAME"
    echo "WISPD_FLAGS=${FLAGS[*]}"; } > "$ENV_FILE"
fi
CUR_FLAGS="$(sed -n 's/^WISPD_FLAGS=//p' "$ENV_FILE")"

# The unit must not depend on a git checkout staying where it is.
install -m 0755 "$REPO/bin/wispd" "$LIB/wispd.new"
mv -f "$LIB/wispd.new" "$LIB/wispd"

# A user unit cannot be ordered after system units, so wait for what the boot
# units provide. Starting early would be worse than starting late: without the
# volume mounted wispd would see an empty sprite directory, and without the
# bridge it would cold-boot every sprite with no NIC.
WAIT=""
if grep -qs -- " $DATA/vm\$" /etc/systemd/system/wisp-storage.service; then
  WAIT+="mountpoint -q '$DATA/vm' && "
fi
if [ -e /etc/systemd/system/wisp-net.service ] && ! grep -q -- '--net=false' <<<"$CUR_FLAGS"; then
  WAIT+="[ -e /sys/class/net/msbr0 ] && "
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

cat > "$UNIT" <<EOF
# Generated by wisp scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=wisp API daemon (wispd, data in $DATA)

[Service]
Type=exec
EnvironmentFile=$ENV_FILE
ExecStartPre=$LIB/wait-host.sh
ExecStart=$LIB/wispd --data $DATA \$WISPD_FLAGS
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

echo "ok: $NAME.service is running and enabled (wispd --data $DATA ${CUR_FLAGS})"
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
